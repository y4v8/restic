package khepri

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"time"

	"github.com/fd0/khepri/backend"

	"code.google.com/p/go.crypto/scrypt"
)

var (
	ErrUnauthenticated = errors.New("Ciphertext verification failed")
	ErrNoKeyFound      = errors.New("No key could be found")
)

// TODO: figure out scrypt values on the fly depending on the current
// hardware.
const (
	scrypt_N        = 65536
	scrypt_r        = 8
	scrypt_p        = 1
	scrypt_saltsize = 64
	aesKeysize      = 32 // for AES256
	hmacKeysize     = 32 // for HMAC with SHA256
)

type Key struct {
	Created  time.Time `json:"created"`
	Username string    `json:"username"`
	Hostname string    `json:"hostname"`
	Comment  string    `json:"comment,omitempty"`

	KDF  string `json:"kdf"`
	N    int    `json:"N"`
	R    int    `json:"r"`
	P    int    `json:"p"`
	Salt []byte `json:"salt"`
	Data []byte `json:"data"`

	user   *keys
	master *keys
}

type keys struct {
	Sign    []byte
	Encrypt []byte
}

func CreateKey(be backend.Server, password string) (*Key, error) {
	// fill meta data about key
	k := &Key{
		Created: time.Now(),
		KDF:     "scrypt",
		N:       scrypt_N,
		R:       scrypt_r,
		P:       scrypt_p,
	}

	hn, err := os.Hostname()
	if err == nil {
		k.Hostname = hn
	}

	usr, err := user.Current()
	if err == nil {
		k.Username = usr.Username
	}

	// generate random salt
	k.Salt = make([]byte, scrypt_saltsize)
	n, err := rand.Read(k.Salt)
	if n != scrypt_saltsize || err != nil {
		panic("unable to read enough random bytes for salt")
	}

	// call scrypt() to derive user key
	k.user, err = k.scrypt(password)
	if err != nil {
		return nil, err
	}

	// generate new random master keys
	k.master, err = k.newKeys()
	if err != nil {
		return nil, err
	}

	// encrypt master keys (as json) with user key
	buf, err := json.Marshal(k.master)
	if err != nil {
		return nil, err
	}

	k.Data, err = k.EncryptUser(buf)

	// dump as json
	buf, err = json.Marshal(k)
	if err != nil {
		return nil, err
	}

	// store in repository and return
	_, err = be.Create(backend.Key, buf)
	if err != nil {
		return nil, err
	}

	return k, nil
}

func OpenKey(be backend.Server, id backend.ID, password string) (*Key, error) {
	// extract data from repo
	data, err := be.Get(backend.Key, id)
	if err != nil {
		return nil, err
	}

	// restore json
	k := &Key{}
	err = json.Unmarshal(data, k)
	if err != nil {
		return nil, err
	}

	// check KDF
	if k.KDF != "scrypt" {
		return nil, errors.New("only supported KDF is scrypt()")
	}

	// derive user key
	k.user, err = k.scrypt(password)
	if err != nil {
		return nil, err
	}

	// decrypt master keys
	buf, err := k.DecryptUser(k.Data)
	if err != nil {
		return nil, err
	}

	// restore json
	k.master = &keys{}
	err = json.Unmarshal(buf, k.master)
	if err != nil {
		return nil, err
	}

	return k, nil
}

func SearchKey(be backend.Server, password string) (*Key, error) {
	// list all keys
	ids, err := be.List(backend.Key)
	if err != nil {
		panic(err)
	}

	// try all keys in repo
	var key *Key
	for _, id := range ids {
		key, err = OpenKey(be, id, password)
		if err != nil {
			continue
		}

		return key, nil
	}

	return nil, ErrNoKeyFound
}

func (k *Key) scrypt(password string) (*keys, error) {
	if len(k.Salt) == 0 {
		return nil, fmt.Errorf("scrypt() called with empty salt")
	}

	keybytes := hmacKeysize + aesKeysize
	scrypt_keys, err := scrypt.Key([]byte(password), k.Salt, k.N, k.R, k.P, keybytes)
	if err != nil {
		return nil, fmt.Errorf("error deriving keys from password: %v", err)
	}

	if len(scrypt_keys) != keybytes {
		return nil, fmt.Errorf("invalid numbers of bytes expanded from scrypt(): %d", len(scrypt_keys))
	}

	ks := &keys{
		Encrypt: scrypt_keys[:aesKeysize],
		Sign:    scrypt_keys[aesKeysize:],
	}
	return ks, nil
}

func (k *Key) newKeys() (*keys, error) {
	ks := &keys{
		Encrypt: make([]byte, aesKeysize),
		Sign:    make([]byte, hmacKeysize),
	}
	n, err := rand.Read(ks.Encrypt)
	if n != aesKeysize || err != nil {
		panic("unable to read enough random bytes for encryption key")
	}
	n, err = rand.Read(ks.Sign)
	if n != hmacKeysize || err != nil {
		panic("unable to read enough random bytes for signing key")
	}

	return ks, nil
}

func (k *Key) newIV() ([]byte, error) {
	buf := make([]byte, aes.BlockSize)
	_, err := io.ReadFull(rand.Reader, buf)
	if err != nil {
		return nil, err
	}

	return buf, nil
}

func (k *Key) pad(plaintext []byte) []byte {
	l := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	if l == 0 {
		l = aes.BlockSize
	}

	if l <= 0 || l > aes.BlockSize {
		panic("invalid padding size")
	}

	return append(plaintext, bytes.Repeat([]byte{byte(l)}, l)...)
}

func (k *Key) unpad(plaintext []byte) []byte {
	l := len(plaintext)
	pad := plaintext[l-1]

	if pad > aes.BlockSize {
		panic(errors.New("padding > BlockSize"))
	}

	if pad == 0 {
		panic(errors.New("invalid padding 0"))
	}

	for i := l - int(pad); i < l; i++ {
		if plaintext[i] != pad {
			panic(errors.New("invalid padding!"))
		}
	}

	return plaintext[:l-int(pad)]
}

// Encrypt encrypts and signs data. Returned is IV || Ciphertext || HMAC. For
// the hash function, SHA256 is used, so the overhead is 16+32=48 byte.
func (k *Key) encrypt(ks *keys, plaintext []byte) ([]byte, error) {
	iv, err := k.newIV()
	if err != nil {
		panic(fmt.Sprintf("unable to generate new random iv: %v", err))
	}

	c, err := aes.NewCipher(ks.Encrypt)
	if err != nil {
		panic(fmt.Sprintf("unable to create cipher: %v", err))
	}

	e := cipher.NewCBCEncrypter(c, iv)
	p := k.pad(plaintext)
	ciphertext := make([]byte, len(p))
	e.CryptBlocks(ciphertext, p)

	ciphertext = append(iv, ciphertext...)

	hm := hmac.New(sha256.New, ks.Sign)

	n, err := hm.Write(ciphertext)
	if err != nil || n != len(ciphertext) {
		panic(fmt.Sprintf("unable to calculate hmac of ciphertext: %v", err))
	}

	return hm.Sum(ciphertext), nil
}

// EncryptUser encrypts and signs data with the user key. Returned is IV ||
// Ciphertext || HMAC. For the hash function, SHA256 is used, so the overhead
// is 16+32=48 byte.
func (k *Key) EncryptUser(plaintext []byte) ([]byte, error) {
	return k.encrypt(k.user, plaintext)
}

// Encrypt encrypts and signs data with the master key. Returned is IV ||
// Ciphertext || HMAC. For the hash function, SHA256 is used, so the overhead
// is 16+32=48 byte.
func (k *Key) Encrypt(plaintext []byte) ([]byte, error) {
	return k.encrypt(k.master, plaintext)
}

// Decrypt verifes and decrypts the ciphertext. Ciphertext must be in the form
// IV || Ciphertext || HMAC.
func (k *Key) decrypt(ks *keys, ciphertext []byte) ([]byte, error) {
	hm := hmac.New(sha256.New, ks.Sign)

	// extract hmac
	l := len(ciphertext) - hm.Size()
	ciphertext, mac := ciphertext[:l], ciphertext[l:]

	// calculate new hmac
	n, err := hm.Write(ciphertext)
	if err != nil || n != len(ciphertext) {
		panic(fmt.Sprintf("unable to calculate hmac of ciphertext, err %v", err))
	}

	// verify hmac
	mac2 := hm.Sum(nil)

	if !hmac.Equal(mac, mac2) {
		return nil, ErrUnauthenticated
	}

	// extract iv
	iv, ciphertext := ciphertext[:aes.BlockSize], ciphertext[aes.BlockSize:]

	// decrypt data
	c, err := aes.NewCipher(ks.Encrypt)
	if err != nil {
		panic(fmt.Sprintf("unable to create cipher: %v", err))
	}

	// decrypt
	e := cipher.NewCBCDecrypter(c, iv)
	plaintext := make([]byte, len(ciphertext))
	e.CryptBlocks(plaintext, ciphertext)

	// remove padding and return
	return k.unpad(plaintext), nil
}

// Decrypt verifes and decrypts the ciphertext with the master key. Ciphertext
// must be in the form IV || Ciphertext || HMAC.
func (k *Key) Decrypt(ciphertext []byte) ([]byte, error) {
	return k.decrypt(k.master, ciphertext)
}

// DecryptUser verifes and decrypts the ciphertext with the master key. Ciphertext
// must be in the form IV || Ciphertext || HMAC.
func (k *Key) DecryptUser(ciphertext []byte) ([]byte, error) {
	return k.decrypt(k.user, ciphertext)
}

// Each calls backend.Each() with the given parameters, Decrypt() on the
// ciphertext and, on successful decryption, f with the plaintext.
func (k *Key) Each(be backend.Server, t backend.Type, f func(backend.ID, []byte, error)) error {
	return backend.Each(be, t, func(id backend.ID, data []byte, e error) {
		if e != nil {
			f(id, nil, e)
			return
		}

		buf, err := k.Decrypt(data)
		if err != nil {
			f(id, nil, err)
			return
		}

		f(id, buf, nil)
	})
}

func (k *Key) String() string {
	if k == nil {
		return "<Key nil>"
	}
	return fmt.Sprintf("<Key of %s@%s, created on %s>", k.Username, k.Hostname, k.Created)
}