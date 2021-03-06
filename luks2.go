package luks

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/xts"
)

// LUKS v2 format is specified here
// https://habd.as/post/external-backup-drive-encryption/assets/luks2_doc_wip.pdf
type headerV2 struct {
	Magic             [6]byte
	Version           uint16
	HeaderSize        uint64
	SequenceId        uint64
	Label             [48]byte
	ChecksumAlgorithm [32]byte
	Salt              [64]byte
	UUID              [40]byte
	SubsystemLabel    [48]byte
	HeaderOffset      uint64
	_                 [184]byte // padding
	Checksum          [64]byte
	// padding of size 7*512
}

type luks2Device struct {
	hdr  *headerV2
	meta *metadata
}

func luks2OpenDevice(f *os.File) (*luks2Device, error) {
	var hdr headerV2

	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	if err := binary.Read(f, binary.BigEndian, &hdr); err != nil {
		return nil, err
	}

	hdrSize := hdr.HeaderSize // size of header + JSON metadata
	if !isPowerOfTwo(uint(hdrSize)) || hdrSize < 16384 || hdrSize > 4194304 {
		return nil, fmt.Errorf("Invalid size of LUKS header: %v", hdrSize)
	}

	// read the whole header
	data := make([]byte, hdrSize)
	if _, err := f.ReadAt(data, 0); err != nil {
		return nil, err
	}

	for i := 0; i < 64; i++ {
		// clear the checksum
		data[int(unsafe.Offsetof(hdr.Checksum))+i] = 0
	}

	// calculate the checksum of the whole header
	var h hash.Hash
	algo := fixedArrayToString(hdr.ChecksumAlgorithm[:])
	switch algo {
	case "sha256":
		h = sha256.New()
	default:
		return nil, fmt.Errorf("Unknown header checksum algorithm: %v", algo)
	}

	h.Write(data)

	checksum := h.Sum(make([]byte, 0))
	expectedChecksum := hdr.Checksum[:h.Size()]
	if !bytes.Equal(checksum, expectedChecksum) {
		return nil, fmt.Errorf("Invalid header checksum")
	}

	var meta metadata
	jsonData := data[4096:]
	jsonData = jsonData[:bytes.IndexByte(jsonData, 0)]

	if err := json.Unmarshal(jsonData, &meta); err != nil {
		return nil, err
	}

	dev := &luks2Device{
		hdr:  &hdr,
		meta: &meta,
	}
	return dev, nil
}

func (d *luks2Device) uuid() string {
	return fixedArrayToString(d.hdr.UUID[:])
}

func (d *luks2Device) unlockKeyslot(f *os.File, keyslotIdx int, passphrase []byte) (*volumeInfo, error) {
	keyslots := d.meta.Keyslots
	if keyslotIdx < 0 || keyslotIdx >= len(keyslots) {
		return nil, fmt.Errorf("keyslot %d is out of range of available slots", keyslotIdx)
	}

	keyslot := keyslots[keyslotIdx]

	afKey, err := deriveLuks2AfKey(keyslot.Kdf, keyslotIdx, passphrase, keyslot.KeySize)
	if err != nil {
		return nil, err
	}
	defer clearSlice(afKey)

	finalKey, err := decryptLuks2VolumeKey(f, keyslotIdx, keyslot, afKey)
	if err != nil {
		return nil, err
	}

	// verify with digest
	digIdx, digInfo := d.findDigestForKeyslot(keyslotIdx)
	if digInfo == nil {
		return nil, fmt.Errorf("No digest is found for keyslot %v", keyslotIdx)
	}

	generatedDigest, err := computeDigestForKey(digInfo, keyslotIdx, finalKey)
	if err != nil {
		return nil, err
	}
	defer clearSlice(generatedDigest)

	expectedDigest, err := base64.StdEncoding.DecodeString(digInfo.Digest)
	if err != nil {
		return nil, fmt.Errorf("keyslotIdx[%v].digest.Digest base64 parsing failed: %v", keyslotIdx, err)
	}
	if !bytes.Equal(generatedDigest, expectedDigest) {
		return nil, ErrPassphraseDoesNotMatch
	}
	clearSlice(generatedDigest)

	if len(digInfo.Segments) != 1 {
		return nil, fmt.Errorf("LUKS partition expects exactly 1 storage segment, got %+v", len(digInfo.Segments))
	}
	seg, err := digInfo.Segments[0].Int64()
	if err != nil {
		return nil, err
	}

	storageSegment := d.meta.Segments[int(seg)]
	offset, err := storageSegment.Offset.Int64()
	if err != nil {
		return nil, err
	}

	var storageSize uint64
	if storageSegment.Size != "dynamic" {
		size, err := strconv.Atoi(storageSegment.Size)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return nil, fmt.Errorf("invalid segment size: %v", size)
		}

		storageSize = uint64(size)
	}

	ivTweak, err := storageSegment.IvTweak.Int64()
	if err != nil {
		return nil, err
	}

	info := &volumeInfo{
		key:               finalKey,
		digestId:          digIdx,
		luksType:          "LUKS2",
		storageSize:       storageSize / uint64(storageSegment.SectorSize),
		storageOffset:     uint64(offset) / uint64(storageSegment.SectorSize),
		storageEncryption: storageSegment.Encryption,
		storageIvTweak:    uint64(ivTweak),
		storageSectorSize: uint64(storageSegment.SectorSize),
	}
	return info, nil
}

func (d *luks2Device) unlockAnyKeyslot(f *os.File, passphrase []byte) (*volumeInfo, error) {
	// first we iterate over "high"-priority slots, then "normal"
	var highPrio, normPrio []int
	for k, v := range d.meta.Keyslots {
		if v.Priority == "2" {
			highPrio = append(highPrio, k)
		} else if v.Priority == "" || v.Priority == "1" {
			normPrio = append(normPrio, k)
		}
	}
	sort.Ints(highPrio)
	sort.Ints(normPrio)
	activeKeyslots := append(highPrio, normPrio...)

	for _, k := range activeKeyslots {
		volumeKey, err := d.unlockKeyslot(f, k, passphrase)
		if err == nil {
			return volumeKey, nil
		} else if err == ErrPassphraseDoesNotMatch {
			continue
		} else {
			return nil, err
		}
	}
	return nil, ErrPassphraseDoesNotMatch
}

func computeDigestForKey(dig *digest, keyslotIdx int, finalKey []byte) ([]byte, error) {
	digSalt, err := base64.StdEncoding.DecodeString(dig.Salt)
	if err != nil {
		return nil, fmt.Errorf("keyslotIdx[%v].digest.salt base64 parsing failed: %v", keyslotIdx, err)
	}

	switch dig.Type {
	case "pbkdf2":
		var h func() hash.Hash
		var size int
		switch dig.Hash {
		case "sha256":
			h = sha256.New
			size = sha256.Size
		default:
			return nil, fmt.Errorf("Unknown digest hash algorithm: %v", dig.Hash)
		}
		return pbkdf2.Key(finalKey, digSalt, int(dig.Iterations), size, h), nil
	default:
		return nil, fmt.Errorf("Unknown digest kdf type: %v", dig.Type)
	}
}

func decryptLuks2VolumeKey(f *os.File, keyslotIdx int, keyslot keyslot, afKey []byte) ([]byte, error) {
	// parse encryption mode for the keyslot area, see crypt_parse_name_and_mode()
	area := keyslot.Area

	// decrypt keyslotIdx area using the derived key
	keyslotSize := area.KeySize * stripesNum

	areaSize, err := area.Size.Int64()
	if err != nil {
		return nil, fmt.Errorf("Invalid keyslotIdx[%v] size value: %v. %v", keyslotIdx, area.Size, err)
	}
	if int64(keyslotSize) > areaSize {
		return nil, fmt.Errorf("keyslot[%v] area size too small, given %v expected at least %v", keyslotIdx, areaSize, keyslotSize)
	}
	if keyslotSize%storageSectorSize != 0 {
		return nil, fmt.Errorf("keyslot[%v] size %v is not multiple of the sector size %v", keyslotIdx, keyslotSize, storageSectorSize)
	}

	keyData := make([]byte, keyslotSize)
	defer clearSlice(keyData)

	keyslotOffset, err := area.Offset.Int64()
	if err != nil {
		return nil, fmt.Errorf("Invalid keyslotIdx[%v] offset: %v. %v", keyslotIdx, area.Offset, err)
	}
	if keyslotOffset%storageSectorSize != 0 {
		return nil, fmt.Errorf("keyslot[%v] offset %v is not aligned to sector size %v", keyslotIdx, keyslotOffset, storageSectorSize)
	}

	if _, err := f.ReadAt(keyData, keyslotOffset); err != nil {
		return nil, err
	}

	ciph, err := buildLuks2AfCipher(area.Encryption, afKey)
	if err != nil {
		return nil, err
	}

	for i := 0; i < int(keyslotSize/storageSectorSize); i++ {
		block := keyData[i*storageSectorSize : (i+1)*storageSectorSize]
		ciph.Decrypt(block, block, uint64(i))
	}

	// anti-forensic merge
	af := keyslot.Af
	if af.Stripes != stripesNum {
		return nil, fmt.Errorf("LUKS currently supports only af with 4000 stripes")
	}
	var afHash hash.Hash
	switch af.Hash {
	case "sha256":
		afHash = sha256.New()
	default:
		return nil, fmt.Errorf("Unknown af hash algorithm: %v", af.Hash)
	}

	return afMerge(keyData, int(keyslot.KeySize), int(af.Stripes), afHash)
}

func buildLuks2AfCipher(encryption string, afKey []byte) (*xts.Cipher, error) {
	// example of `encryption` value is 'aes-xts-plain64'
	encParts := strings.Split(encryption, "-")
	if len(encParts) != 3 {
		return nil, fmt.Errorf("Unexpected encryption format: %v", encryption)
	}
	cipherName := encParts[0]
	cipherMode := encParts[1]
	// ivModeName := encParts[2]

	var cipherFunc func(key []byte) (cipher.Block, error)
	switch cipherName {
	case "aes":
		cipherFunc = aes.NewCipher
	default:
		return nil, fmt.Errorf("Unknown cipher: %v", cipherName)
	}

	switch cipherMode {
	case "xts":
		return xts.NewCipher(cipherFunc, afKey)
	default:
		return nil, fmt.Errorf("Unknown encryption mode: %v", cipherMode)
	}
}

func deriveLuks2AfKey(kdf kdf, keyslotIdx int, passphrase []byte, keyLength uint) ([]byte, error) {
	salt, err := base64.StdEncoding.DecodeString(kdf.Salt)
	if err != nil {
		return nil, fmt.Errorf("keyslotIdx[%v].kdf.salt base64 parsing failed: %v", keyslotIdx, err)
	}

	switch kdf.Type {
	case "pbkdf2":
		var h func() hash.Hash
		switch kdf.Hash {
		case "sha256":
			h = sha256.New
		default:
			return nil, fmt.Errorf("Unknown keyslotIdx[%v].kdf.hash algorithm: %v", keyslotIdx, kdf.Hash)
		}
		return pbkdf2.Key(passphrase, salt, int(kdf.Iterations), int(keyLength), h), nil
	case "argon2i":
		return argon2.Key(passphrase, salt, uint32(kdf.Time), uint32(kdf.Memory), uint8(kdf.Cpus), uint32(keyLength)), nil
	case "argon2id":
		return argon2.IDKey(passphrase, salt, uint32(kdf.Time), uint32(kdf.Memory), uint8(kdf.Cpus), uint32(keyLength)), nil
	default:
		return nil, fmt.Errorf("Unknown kdf type: %v", kdf.Type)
	}
}

func (d *luks2Device) findDigestForKeyslot(keyslotIdx int) (int, *digest) {
	for i, dig := range d.meta.Digests {
		for _, k := range dig.Keyslots {
			k, e := k.Int64()
			if e != nil {
				continue
			}
			if int(k) == keyslotIdx {
				return i, &dig
			}
		}
	}
	return 0, nil
}
