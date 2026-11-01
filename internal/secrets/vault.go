// Package secrets stores profile credentials encrypted with a separately held key.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Vault struct {
	Key  string
	Path string
	mu   sync.Mutex
}

func FromEnv() *Vault {
	path := os.Getenv("AUTOBID_LLM_KEY_FILE")
	if path == "" {
		path = filepath.Join("data", "llm-master.key")
	}
	return &Vault{Key: os.Getenv("AUTOBID_LLM_MASTER_KEY"), Path: path}
}
func (v *Vault) key(create bool) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.Key != "" {
		b, e := base64.StdEncoding.DecodeString(v.Key)
		if e != nil || len(b) != 32 {
			return nil, errors.New("AUTOBID_LLM_MASTER_KEY must be base64 of 32 bytes")
		}
		return b, nil
	}
	b, err := os.ReadFile(v.Path)
	if errors.Is(err, os.ErrNotExist) && create {
		if err = os.MkdirAll(filepath.Dir(v.Path), 0700); err != nil {
			return nil, errors.New("cannot create credential key directory")
		}
		b = make([]byte, 32)
		if _, err = rand.Read(b); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(v.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errors.Is(e, os.ErrExist) {
			b, err = os.ReadFile(v.Path)
		} else if e != nil {
			return nil, e
		} else {
			_, err = f.Write(b)
			if err == nil {
				err = f.Sync()
			}
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		return nil, errors.New("credential master key unavailable; restore the original key file")
	}
	if len(b) != 32 {
		return nil, errors.New("invalid credential master key file")
	}
	return b, nil
}
func (v *Vault) aead(create bool) (cipher.AEAD, error) {
	k, err := v.key(create)
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}
func (v *Vault) Encrypt(plain string) (string, error) {
	a, err := v.aead(true)
	if err != nil {
		return "", err
	}
	n := make([]byte, a.NonceSize())
	if _, err = rand.Read(n); err != nil {
		return "", err
	}
	return "v1:" + base64.StdEncoding.EncodeToString(a.Seal(n, n, []byte(plain), []byte("autobid-llm-key-v1"))), nil
}
func (v *Vault) Decrypt(value string) (string, error) {
	if len(value) < 3 || value[:3] != "v1:" {
		return "", errors.New("unsupported credential encoding")
	}
	a, err := v.aead(false)
	if err != nil {
		return "", err
	}
	b, err := base64.StdEncoding.DecodeString(value[3:])
	if err != nil || len(b) < a.NonceSize() {
		return "", errors.New("invalid encrypted credential")
	}
	p, err := a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], []byte("autobid-llm-key-v1"))
	if err != nil {
		return "", fmt.Errorf("credential cannot be decrypted; restore original master key")
	}
	return string(p), nil
}
