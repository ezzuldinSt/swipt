package automation

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"slices"
	"strings"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/jsc"

	"filippo.io/bigmod"
)

type cryptoParams struct {
	Name           string `json:"name"`
	Hash           string `json:"hash"`
	NamedCurve     string `json:"namedCurve"`
	ModulusLength  int    `json:"modulusLength"`
	PublicExponent []byte `json:"publicExponent"`
	SaltLength     int    `json:"saltLength"`
	Label          []byte `json:"label"`
	IV             []byte `json:"iv"`
	Counter        []byte `json:"counter"`
	Length         int    `json:"length"`
	AdditionalData []byte `json:"additionalData"`
	TagLength      int    `json:"tagLength"`
	Salt           []byte `json:"salt"`
	Info           []byte `json:"info"`
	Iterations     int    `json:"iterations"`
	Public         []byte `json:"public"`
}

type cryptoJWK struct {
	Kty string        `json:"kty"`
	Crv string        `json:"crv,omitempty"`
	X   string        `json:"x,omitempty"`
	Y   string        `json:"y,omitempty"`
	D   string        `json:"d,omitempty"`
	N   string        `json:"n,omitempty"`
	E   string        `json:"e,omitempty"`
	P   string        `json:"p,omitempty"`
	Q   string        `json:"q,omitempty"`
	DP  string        `json:"dp,omitempty"`
	DQ  string        `json:"dq,omitempty"`
	QI  string        `json:"qi,omitempty"`
	Oth []cryptoPrime `json:"oth,omitempty"`
}

type cryptoPrime struct {
	R string `json:"r,omitempty"`
	D string `json:"d,omitempty"`
	T string `json:"t,omitempty"`
}

type pkcs8Envelope struct {
	Version    int
	Algorithm  pkix.AlgorithmIdentifier
	PrivateKey []byte
}

type pkcs1Key struct {
	Version          int
	N                *big.Int
	E                int
	D                *big.Int
	P                *big.Int
	Q                *big.Int
	Dp               *big.Int
	Dq               *big.Int
	Qinv             *big.Int
	AdditionalPrimes []asn1.RawValue `asn1:"optional,omitempty"`
}

type cryptoKeyPair struct {
	Private []byte `json:"private"`
	Public  []byte `json:"public"`
}

type cryptoImported struct {
	Type           string `json:"type"`
	Material       []byte `json:"material"`
	ModulusLength  int    `json:"modulusLength,omitempty"`
	PublicExponent []byte `json:"publicExponent,omitempty"`
}

var (
	kwIV   = []byte{0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6}
	oidRSA = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidEC  = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	bigOne = big.NewInt(1)
)

func cryptoHash(name string) (crypto.Hash, error) {
	switch name {
	case "SHA-1":
		return crypto.SHA1, nil
	case "SHA-256":
		return crypto.SHA256, nil
	case "SHA-384":
		return crypto.SHA384, nil
	case "SHA-512":
		return crypto.SHA512, nil
	}
	return 0, E.New("unknown hash ", name)
}

func cryptoCurve(name string) (elliptic.Curve, error) {
	switch name {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	}
	return nil, E.New("unknown curve ", name)
}

func cryptoDigest(name string, data []byte) (crypto.Hash, []byte, error) {
	hash, err := cryptoHash(name)
	if err != nil {
		return 0, nil, err
	}
	h := hash.New()
	h.Write(data)
	return hash, h.Sum(nil), nil
}

func (o *operation) cryptoInput(extra ...any) (cryptoParams, []byte, []byte, error) {
	var params cryptoParams
	var material []byte
	var key string
	if err := o.decode(3+len(extra), append([]any{&params, &material, &key}, extra...)...); err != nil {
		return params, nil, nil, err
	}
	data, err := o.engine.takeBlob(key)
	return params, material, data, err
}

func cryptoEncrypt(_ context.Context, call *operation) (any, error) {
	params, material, data, err := call.cryptoInput()
	if err != nil {
		return nil, err
	}
	if params.Name == "RSA-OAEP" {
		key, err := x509.ParsePKIXPublicKey(material)
		if err != nil {
			return nil, err
		}
		public, isRSA := key.(*rsa.PublicKey)
		if !isRSA {
			return nil, E.New("the key is not an RSA public key")
		}
		hash, err := cryptoHash(params.Hash)
		if err != nil {
			return nil, err
		}
		output, err := rsa.EncryptOAEP(hash.New(), rand.Reader, public, data, params.Label)
		return jsc.Bytes(output), err
	}
	output, err := aesCipher(params, material, data, true)
	return jsc.Bytes(output), err
}

func cryptoDecrypt(_ context.Context, call *operation) (any, error) {
	params, material, data, err := call.cryptoInput()
	if err != nil {
		return nil, err
	}
	if params.Name == "RSA-OAEP" {
		key, err := operationKey(material)
		if err != nil {
			return nil, err
		}
		private, isRSA := key.(*rsa.PrivateKey)
		if !isRSA {
			return nil, E.New("the key is not an RSA private key")
		}
		hash, err := cryptoHash(params.Hash)
		if err != nil {
			return nil, err
		}
		output, err := rsa.DecryptOAEP(hash.New(), nil, private, data, params.Label)
		if err != nil {
			return nil, E.New("the data could not be decrypted")
		}
		return jsc.Bytes(output), nil
	}
	output, err := aesCipher(params, material, data, false)
	return jsc.Bytes(output), err
}

func aesCipher(params cryptoParams, key, data []byte, encrypt bool) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	switch params.Name {
	case "AES-CBC":
		size := block.BlockSize()
		if encrypt {
			padding := size - len(data)%size
			output := append(slices.Clone(data), bytes.Repeat([]byte{byte(padding)}, padding)...)
			cipher.NewCBCEncrypter(block, params.IV).CryptBlocks(output, output)
			return output, nil
		}
		if len(data) == 0 || len(data)%size != 0 {
			return nil, E.New("the data is not a whole number of blocks")
		}
		output := make([]byte, len(data))
		cipher.NewCBCDecrypter(block, params.IV).CryptBlocks(output, data)
		padding := int(output[len(output)-1])
		if padding == 0 || padding > size || !bytes.Equal(output[len(output)-padding:], bytes.Repeat([]byte{byte(padding)}, padding)) {
			return nil, E.New("the padding is invalid")
		}
		return output[:len(output)-padding], nil
	case "AES-CTR":
		return aesCTR(block, params.Counter, params.Length, data)
	case "AES-GCM":
		return aesGCM(block, params, data, encrypt)
	case "AES-KW":
		if encrypt {
			return aesWrap(block, data)
		}
		return aesUnwrap(block, data)
	}
	return nil, E.New("unknown cipher ", params.Name)
}

func aesCTR(block cipher.Block, counter []byte, bits int, data []byte) ([]byte, error) {
	size := block.BlockSize()
	if blocks := (len(data) + size - 1) / size; bits < 63 && int64(blocks) > int64(1)<<bits {
		return nil, E.New("the data is longer than the counter allows")
	}
	output := make([]byte, len(data))
	current := slices.Clone(counter)
	stream := make([]byte, size)
	for offset := 0; offset < len(data); offset += size {
		block.Encrypt(stream, current)
		subtle.XORBytes(output[offset:], data[offset:], stream)
		for index, remaining := size-1, bits; index >= 0 && remaining > 0; index, remaining = index-1, remaining-8 {
			mask := byte(0xff)
			if remaining < 8 {
				mask = byte(1)<<remaining - 1
			}
			next := (current[index]&mask + 1) & mask
			current[index] = current[index]&^mask | next
			if next != 0 {
				break
			}
		}
	}
	return output, nil
}

func aesGCM(block cipher.Block, params cryptoParams, data []byte, encrypt bool) ([]byte, error) {
	aead, err := cipher.NewGCMWithNonceSize(block, len(params.IV))
	if err != nil {
		return nil, err
	}
	tagSize := params.TagLength / 8
	if encrypt {
		sealed := aead.Seal(nil, params.IV, data, params.AdditionalData)
		return sealed[:len(sealed)-aead.Overhead()+tagSize], nil
	}
	if len(data) < tagSize {
		return nil, E.New("the data is shorter than the tag")
	}
	if tagSize == aead.Overhead() {
		output, err := aead.Open(nil, params.IV, data, params.AdditionalData)
		if err != nil {
			return nil, E.New("the data could not be authenticated")
		}
		return output, nil
	}
	ciphertext, tag := data[:len(data)-tagSize], data[len(data)-tagSize:]
	output := aead.Seal(nil, params.IV, ciphertext, nil)[:len(ciphertext)]
	sealed := aead.Seal(nil, params.IV, output, params.AdditionalData)
	if subtle.ConstantTimeCompare(sealed[len(ciphertext):len(ciphertext)+tagSize], tag) != 1 {
		return nil, E.New("the data could not be authenticated")
	}
	return output, nil
}

func aesWrap(block cipher.Block, data []byte) ([]byte, error) {
	if len(data) < 16 || len(data)%8 != 0 {
		return nil, E.New("the data is not a multiple of 8 bytes of at least 16 bytes")
	}
	count := len(data) / 8
	output := append(slices.Clone(kwIV), data...)
	buffer := make([]byte, 16)
	for round := range 6 {
		for index := 1; index <= count; index++ {
			copy(buffer, output[:8])
			copy(buffer[8:], output[index*8:index*8+8])
			block.Encrypt(buffer, buffer)
			binary.BigEndian.PutUint64(output[:8], binary.BigEndian.Uint64(buffer[:8])^uint64(count*round+index))
			copy(output[index*8:], buffer[8:])
		}
	}
	return output, nil
}

func aesUnwrap(block cipher.Block, data []byte) ([]byte, error) {
	if len(data) < 24 || len(data)%8 != 0 {
		return nil, E.New("the data is not a multiple of 8 bytes of at least 24 bytes")
	}
	count := len(data)/8 - 1
	output := slices.Clone(data)
	buffer := make([]byte, 16)
	for round := 5; round >= 0; round-- {
		for index := count; index >= 1; index-- {
			binary.BigEndian.PutUint64(buffer, binary.BigEndian.Uint64(output[:8])^uint64(count*round+index))
			copy(buffer[8:], output[index*8:index*8+8])
			block.Decrypt(buffer, buffer)
			copy(output[:8], buffer[:8])
			copy(output[index*8:], buffer[8:])
		}
	}
	if subtle.ConstantTimeCompare(output[:8], kwIV) != 1 {
		return nil, E.New("the data could not be unwrapped")
	}
	return output[8:], nil
}

func cryptoSign(_ context.Context, call *operation) (any, error) {
	params, material, data, err := call.cryptoInput()
	if err != nil {
		return nil, err
	}
	if params.Name == "HMAC" {
		sum, err := hmacSum(params.Hash, material, data)
		return jsc.Bytes(sum), err
	}
	key, err := operationKey(material)
	if err != nil {
		return nil, err
	}
	if private, isEd25519 := key.(ed25519.PrivateKey); isEd25519 {
		return jsc.Bytes(ed25519.Sign(private, data)), nil
	}
	hash, digest, err := cryptoDigest(params.Hash, data)
	if err != nil {
		return nil, err
	}
	switch private := key.(type) {
	case *rsa.PrivateKey:
		if params.Name == "RSA-PSS" {
			if params.SaltLength == 0 {
				return signPSSWithoutSalt(private, hash, digest)
			}
			signature, err := rsa.SignPSS(rand.Reader, private, hash, digest, &rsa.PSSOptions{SaltLength: params.SaltLength, Hash: hash})
			return jsc.Bytes(signature), err
		}
		signature, err := rsa.SignPKCS1v15(nil, private, hash, digest)
		return jsc.Bytes(signature), err
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, private, digest)
		if err != nil {
			return nil, err
		}
		size := (private.Curve.Params().BitSize + 7) / 8
		signature := make([]byte, 2*size)
		r.FillBytes(signature[:size])
		s.FillBytes(signature[size:])
		return jsc.Bytes(signature), nil
	}
	return nil, E.New("the key does not sign")
}

func operationKey(material []byte) (crypto.PrivateKey, error) {
	var envelope pkcs8Envelope
	var key pkcs1Key
	if _, err := asn1.Unmarshal(material, &envelope); err == nil && envelope.Algorithm.Algorithm.Equal(oidRSA) {
		if _, err = asn1.Unmarshal(envelope.PrivateKey, &key); err == nil && len(key.AdditionalPrimes) > 0 {
			return &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: key.N, E: key.E}, D: key.D}, nil
		}
	}
	return x509.ParsePKCS8PrivateKey(material)
}

func encodePSSWithoutSalt(hash crypto.Hash, digest []byte, bits int) ([]byte, error) {
	size := hash.Size()
	length := (bits + 7) / 8
	if length < size+2 {
		return nil, E.New("the key is too short for the hash")
	}
	h := hash.New()
	h.Write(make([]byte, 8))
	h.Write(digest)
	sum := h.Sum(nil)
	block := make([]byte, length-size-1)
	block[len(block)-1] = 1
	var counter [4]byte
	for offset := 0; offset < len(block); {
		h.Reset()
		h.Write(sum)
		h.Write(counter[:])
		offset += subtle.XORBytes(block[offset:], block[offset:], h.Sum(nil))
		binary.BigEndian.PutUint32(counter[:], binary.BigEndian.Uint32(counter[:])+1)
	}
	block[0] &= 0xff >> (8*length - bits)
	return slices.Concat(block, sum, []byte{0xbc}), nil
}

func signPSSWithoutSalt(private *rsa.PrivateKey, hash crypto.Hash, digest []byte) (jsc.Bytes, error) {
	if private.N.BitLen() < 1024 {
		return nil, E.New("the key is shorter than 1024 bits")
	}
	encoded, err := encodePSSWithoutSalt(hash, digest, private.N.BitLen()-1)
	if err != nil {
		return nil, err
	}
	modulus, err := bigmod.NewModulus(private.N.Bytes())
	if err != nil {
		return nil, err
	}
	message, err := bigmod.NewNat().SetBytes(encoded, modulus)
	if err != nil {
		return nil, err
	}
	signature := bigmod.NewNat().Exp(message, private.D.Bytes(), modulus)
	if bigmod.NewNat().ExpShortVarTime(signature, uint(private.E), modulus).Equal(message) != 1 {
		return nil, E.New("the key is inconsistent")
	}
	return signature.Bytes(modulus), nil
}

func verifyPSSWithoutSalt(public *rsa.PublicKey, hash crypto.Hash, digest, signature []byte) bool {
	bits := public.N.BitLen() - 1
	if public.N.BitLen() < 1024 || len(signature) != (public.N.BitLen()+7)/8 {
		return false
	}
	value := new(big.Int).SetBytes(signature)
	if value.Cmp(public.N) >= 0 {
		return false
	}
	value.Exp(value, big.NewInt(int64(public.E)), public.N)
	expected, err := encodePSSWithoutSalt(hash, digest, bits)
	if err != nil || value.BitLen() > len(expected)*8 {
		return false
	}
	return bytes.Equal(value.FillBytes(make([]byte, len(expected))), expected)
}

func hmacSum(hashName string, key, data []byte) ([]byte, error) {
	hash, err := cryptoHash(hashName)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(hash.New, key)
	mac.Write(data)
	return mac.Sum(nil), nil
}

func cryptoVerify(_ context.Context, call *operation) (any, error) {
	var signature []byte
	params, material, data, err := call.cryptoInput(&signature)
	if err != nil {
		return nil, err
	}
	if params.Name == "HMAC" {
		sum, err := hmacSum(params.Hash, material, data)
		return err == nil && hmac.Equal(sum, signature), err
	}
	key, err := x509.ParsePKIXPublicKey(material)
	if err != nil {
		return nil, err
	}
	if public, isEd25519 := key.(ed25519.PublicKey); isEd25519 {
		if ed25519SmallOrder(public) || len(signature) == ed25519.SignatureSize && ed25519SmallOrder(signature[:32]) {
			return false, nil
		}
		return ed25519.Verify(public, data, signature), nil
	}
	hash, digest, err := cryptoDigest(params.Hash, data)
	if err != nil {
		return nil, err
	}
	switch public := key.(type) {
	case *rsa.PublicKey:
		if params.Name == "RSA-PSS" {
			if params.SaltLength == 0 {
				return verifyPSSWithoutSalt(public, hash, digest, signature), nil
			}
			return rsa.VerifyPSS(public, hash, digest, signature, &rsa.PSSOptions{SaltLength: params.SaltLength, Hash: hash}) == nil, nil
		}
		return rsa.VerifyPKCS1v15(public, hash, digest, signature) == nil, nil
	case *ecdsa.PublicKey:
		size := (public.Curve.Params().BitSize + 7) / 8
		if len(signature) != 2*size {
			return false, nil
		}
		return ecdsa.Verify(public, digest, new(big.Int).SetBytes(signature[:size]), new(big.Int).SetBytes(signature[size:])), nil
	}
	return nil, E.New("the key does not verify")
}

func cryptoDerive(_ context.Context, call *operation) (any, error) {
	var params cryptoParams
	var material []byte
	var length *int
	if err := call.decode(2, &params, &material, &length); err != nil {
		return nil, err
	}
	if length == nil && (params.Name == "HKDF" || params.Name == "PBKDF2") {
		return nil, E.New("the length is required")
	}
	switch params.Name {
	case "HKDF":
		hash, err := cryptoHash(params.Hash)
		if err != nil {
			return nil, err
		}
		output, err := hkdf.Key(hash.New, material, params.Salt, string(params.Info), *length/8)
		return jsc.Bytes(output), err
	case "PBKDF2":
		hash, err := cryptoHash(params.Hash)
		if err != nil {
			return nil, err
		}
		if *length == 0 {
			return jsc.Bytes{}, nil
		}
		output, err := pbkdf2.Key(hash.New, string(material), params.Salt, params.Iterations, *length/8)
		return jsc.Bytes(output), err
	}
	private, err := ecdhPrivateKey(material)
	if err != nil {
		return nil, err
	}
	public, err := ecdhPublicKey(params.Public)
	if err != nil {
		return nil, err
	}
	secret, err := private.ECDH(public)
	if err != nil {
		return nil, err
	}
	if length == nil {
		return jsc.Bytes(secret), nil
	}
	if *length > len(secret)*8 {
		return nil, E.New("the length is longer than the ", len(secret)*8, "-bit shared secret")
	}
	output := secret[:(*length+7)/8]
	if remainder := *length % 8; remainder != 0 {
		output[len(output)-1] &= byte(0xff) << (8 - remainder)
	}
	return jsc.Bytes(output), nil
}

func ecdhPrivateKey(material []byte) (*ecdh.PrivateKey, error) {
	key, err := x509.ParsePKCS8PrivateKey(material)
	if err != nil {
		return nil, err
	}
	switch private := key.(type) {
	case *ecdh.PrivateKey:
		return private, nil
	case *ecdsa.PrivateKey:
		return private.ECDH()
	}
	return nil, E.New("the key does not derive")
}

func ecdhPublicKey(material []byte) (*ecdh.PublicKey, error) {
	key, err := x509.ParsePKIXPublicKey(material)
	if err != nil {
		return nil, err
	}
	switch public := key.(type) {
	case *ecdh.PublicKey:
		return public, nil
	case *ecdsa.PublicKey:
		return public.ECDH()
	}
	return nil, E.New("the public key does not derive")
}

func cryptoGenerate(ctx context.Context, call *operation) (any, error) {
	var params cryptoParams
	if err := call.decode(1, &params); err != nil {
		return nil, err
	}
	var private crypto.PrivateKey
	var err error
	switch params.Name {
	case "RSASSA-PKCS1-v1_5", "RSA-PSS", "RSA-OAEP":
		private, err = generateRSA(ctx, params.ModulusLength, params.PublicExponent)
	case "ECDSA", "ECDH":
		var curve elliptic.Curve
		curve, err = cryptoCurve(params.NamedCurve)
		if err != nil {
			return nil, err
		}
		private, err = ecdsa.GenerateKey(curve, rand.Reader)
	case "Ed25519":
		_, private, err = ed25519.GenerateKey(rand.Reader)
	case "X25519":
		private, err = ecdh.X25519().GenerateKey(rand.Reader)
	default:
		return nil, E.New("unknown key pair ", params.Name)
	}
	if err != nil {
		return nil, err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(private.(interface{ Public() crypto.PublicKey }).Public())
	if err != nil {
		return nil, err
	}
	return cryptoKeyPair{Private: privateDER, Public: publicDER}, nil
}

func generateRSA(ctx context.Context, bits int, exponent []byte) (*rsa.PrivateKey, error) {
	e := new(big.Int).SetBytes(exponent)
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 || e.Bit(0) == 0 {
		return nil, E.New("the public exponent is not an odd number from 3 to 2^31-1")
	}
	if bits > 16384 {
		return nil, E.New("the modulus is longer than 16384 bits")
	}
	for {
		key, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil || e.Int64() == 65537 {
			return key, err
		}
		p, q := new(big.Int).Sub(key.Primes[0], bigOne), new(big.Int).Sub(key.Primes[1], bigOne)
		lambda := new(big.Int).Mul(p, q)
		lambda.Div(lambda, new(big.Int).GCD(nil, nil, p, q))
		if d := new(big.Int).ModInverse(e, lambda); d != nil {
			private := &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: key.N, E: int(e.Int64())}, D: d, Primes: key.Primes}
			private.Precompute()
			return private, private.Validate()
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func cryptoImport(_ context.Context, call *operation) (any, error) {
	var format string
	var params cryptoParams
	var data json.RawMessage
	if err := call.decode(3, &format, &params, &data); err != nil {
		return nil, err
	}
	var key any
	var err error
	if format == "jwk" {
		var jwk cryptoJWK
		if err = json.Unmarshal(data, &jwk); err != nil {
			return nil, err
		}
		key, err = jwkKey(params, jwk)
	} else {
		var der []byte
		if err = json.Unmarshal(data, &der); err != nil {
			return nil, err
		}
		switch format {
		case "spki":
			if key, err = spkiPublicKey(der); err != nil {
				err = E.New("the data is not a supported SubjectPublicKeyInfo")
			}
		case "pkcs8":
			if key, err = x509.ParsePKCS8PrivateKey(der); err != nil {
				err = E.New("the data is not a supported PKCS #8 private key")
			}
		case "raw":
			key, err = rawPublicKey(params, der)
		default:
			err = E.New("unknown format ", format)
		}
	}
	if err != nil {
		return nil, err
	}
	return importedKey(params, key)
}

// uncompressedPoint decompresses an EC point in SEC 1's compressed form and keeps any other form as it is.
func uncompressedPoint(curve elliptic.Curve, data []byte) ([]byte, error) {
	if len(data) == 0 || data[0] != 2 && data[0] != 3 {
		return data, nil
	}
	x, y := elliptic.UnmarshalCompressed(curve, data)
	if x == nil {
		return nil, E.New("the data is not a point on ", curve.Params().Name)
	}
	size := (curve.Params().BitSize + 7) / 8
	return slices.Concat([]byte{4}, x.FillBytes(make([]byte, size)), y.FillBytes(make([]byte, size))), nil
}

// spkiPublicKey parses a SubjectPublicKeyInfo. x509 takes only uncompressed EC points, so a compressed one is decompressed first.
func spkiPublicKey(der []byte) (any, error) {
	key, err := x509.ParsePKIXPublicKey(der)
	if err == nil {
		return key, nil
	}
	var info struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	var named asn1.ObjectIdentifier
	if rest, parseErr := asn1.Unmarshal(der, &info); parseErr != nil || len(rest) > 0 || !info.Algorithm.Algorithm.Equal(oidEC) {
		return nil, err
	}
	if _, parseErr := asn1.Unmarshal(info.Algorithm.Parameters.FullBytes, &named); parseErr != nil {
		return nil, err
	}
	var curve elliptic.Curve
	for _, candidate := range []elliptic.Curve{elliptic.P256(), elliptic.P384(), elliptic.P521()} {
		if curveOID(candidate).Equal(named) {
			curve = candidate
		}
	}
	point := info.PublicKey.RightAlign()
	if curve == nil || len(point) == 0 || point[0] != 2 && point[0] != 3 {
		return nil, err
	}
	if point, err = uncompressedPoint(curve, point); err != nil {
		return nil, err
	}
	info.PublicKey = asn1.BitString{Bytes: point, BitLength: len(point) * 8}
	if der, err = asn1.Marshal(info); err != nil {
		return nil, err
	}
	return x509.ParsePKIXPublicKey(der)
}

func curveOID(curve elliptic.Curve) asn1.ObjectIdentifier {
	switch curve {
	case elliptic.P256():
		return asn1.ObjectIdentifier{1, 2, 840, 10045, 3, 1, 7}
	case elliptic.P384():
		return asn1.ObjectIdentifier{1, 3, 132, 0, 34}
	}
	return asn1.ObjectIdentifier{1, 3, 132, 0, 35}
}

func rawPublicKey(params cryptoParams, data []byte) (any, error) {
	switch params.Name {
	case "ECDSA", "ECDH":
		curve, err := cryptoCurve(params.NamedCurve)
		if err != nil {
			return nil, err
		}
		if data, err = uncompressedPoint(curve, data); err != nil {
			return nil, err
		}
		return ecdsa.ParseUncompressedPublicKey(curve, data)
	case "Ed25519":
		if len(data) != ed25519.PublicKeySize {
			return nil, E.New("the key is not ", ed25519.PublicKeySize, " bytes")
		}
		return ed25519.PublicKey(data), nil
	case "X25519":
		return ecdh.X25519().NewPublicKey(data)
	}
	return nil, E.New("unknown key pair ", params.Name)
}

func importedKey(params cryptoParams, key any) (*cryptoImported, error) {
	var family string
	var private bool
	switch k := key.(type) {
	case *rsa.PrivateKey:
		family, private = "RSA", true
	case *rsa.PublicKey:
		family = "RSA"
	case *ecdsa.PrivateKey:
		family, private = k.Curve.Params().Name, true
	case *ecdsa.PublicKey:
		family = k.Curve.Params().Name
	case ed25519.PrivateKey:
		family, private = "Ed25519", true
	case ed25519.PublicKey:
		family = "Ed25519"
	case *ecdh.PrivateKey:
		family, private = curveName(k.Curve()), true
	case *ecdh.PublicKey:
		family = curveName(k.Curve())
	}
	var expected string
	switch params.Name {
	case "RSASSA-PKCS1-v1_5", "RSA-PSS", "RSA-OAEP":
		expected = "RSA"
	case "ECDSA", "ECDH":
		expected = params.NamedCurve
	default:
		expected = params.Name
	}
	if family != expected {
		return nil, E.New("the key is not a ", expected, " key")
	}
	if k, isRSA := key.(*rsa.PrivateKey); isRSA && len(k.Primes) > 2 {
		if err := checkMultiPrime(k); err != nil {
			return nil, err
		}
	}
	result := &cryptoImported{Type: "public"}
	var err error
	if private {
		result.Type = "private"
		result.Material, err = x509.MarshalPKCS8PrivateKey(key)
	} else {
		result.Material, err = x509.MarshalPKIXPublicKey(key)
	}
	if err != nil {
		return nil, err
	}
	var public *rsa.PublicKey
	switch k := key.(type) {
	case *rsa.PrivateKey:
		public = &k.PublicKey
	case *rsa.PublicKey:
		public = k
	}
	if public != nil {
		result.ModulusLength = public.N.BitLen()
		result.PublicExponent = big.NewInt(int64(public.E)).Bytes()
	}
	return result, nil
}

func checkMultiPrime(key *rsa.PrivateKey) error {
	product := new(big.Int).Set(bigOne)
	ed := new(big.Int).Mul(key.D, big.NewInt(int64(key.E)))
	for index, prime := range key.Primes {
		if prime.Cmp(bigOne) <= 0 || slices.ContainsFunc(key.Primes[:index], func(other *big.Int) bool { return other.Cmp(prime) == 0 }) {
			return E.New("the primes are not distinct and greater than 1")
		}
		product.Mul(product, prime)
		if new(big.Int).Mod(ed, new(big.Int).Sub(prime, bigOne)).Cmp(bigOne) != 0 {
			return E.New("d is inconsistent with the primes")
		}
	}
	if product.Cmp(key.N) != 0 {
		return E.New("the primes do not multiply to n")
	}
	digest := sha256.Sum256(key.N.Bytes())
	signature, err := rsa.SignPKCS1v15(nil, &rsa.PrivateKey{PublicKey: key.PublicKey, D: key.D}, crypto.SHA256, digest[:])
	if err != nil {
		return err
	}
	return rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature)
}

func otherPrimes(key *rsa.PrivateKey) [][3]*big.Int {
	values := make([][3]*big.Int, 0, len(key.Primes)-2)
	product := new(big.Int).Mul(key.Primes[0], key.Primes[1])
	for _, prime := range key.Primes[2:] {
		exponent := new(big.Int).Mod(key.D, new(big.Int).Sub(prime, bigOne))
		values = append(values, [3]*big.Int{prime, exponent, new(big.Int).ModInverse(product, prime)})
		product = new(big.Int).Mul(product, prime)
	}
	return values
}

func curveName(curve ecdh.Curve) string {
	if curve == ecdh.X25519() {
		return "X25519"
	}
	return ""
}

func jwkKey(params cryptoParams, jwk cryptoJWK) (any, error) {
	var failure error
	decode := func(name, value string) []byte {
		if failure != nil {
			return nil
		}
		if value == "" {
			failure = E.New("the JWK has no ", name)
			return nil
		}
		data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(value, "="))
		if err != nil {
			failure = E.Cause(err, "decode the JWK ", name)
		}
		return data
	}
	integer := func(name, value string) *big.Int {
		return new(big.Int).SetBytes(decode(name, value))
	}
	switch params.Name {
	case "RSASSA-PKCS1-v1_5", "RSA-PSS", "RSA-OAEP":
		if jwk.Kty != "RSA" {
			return nil, E.New("the JWK kty is not RSA")
		}
		exponent := integer("e", jwk.E)
		public := rsa.PublicKey{N: integer("n", jwk.N), E: int(exponent.Int64())}
		if failure == nil && (!exponent.IsInt64() || exponent.Int64() > 1<<31-1) {
			failure = E.New("the JWK e is too large")
		}
		if jwk.D == "" {
			return &public, failure
		}
		crt := rsa.PrecomputedValues{Dp: integer("dp", jwk.DP), Dq: integer("dq", jwk.DQ), Qinv: integer("qi", jwk.QI)}
		private := &rsa.PrivateKey{PublicKey: public, D: integer("d", jwk.D), Primes: []*big.Int{integer("p", jwk.P), integer("q", jwk.Q)}}
		others := make([][3]*big.Int, len(jwk.Oth))
		for index, other := range jwk.Oth {
			others[index] = [3]*big.Int{integer("oth r", other.R), integer("oth d", other.D), integer("oth t", other.T)}
			private.Primes = append(private.Primes, others[index][0])
		}
		if failure != nil {
			return nil, failure
		}
		if len(others) == 0 {
			private.Precomputed = crt
			private.Precompute()
			return private, private.Validate()
		}
		private.Precompute()
		if err := checkMultiPrime(private); err != nil {
			return nil, err
		}
		computed := otherPrimes(private)
		if private.Precomputed.Dp.Cmp(crt.Dp) != 0 || private.Precomputed.Dq.Cmp(crt.Dq) != 0 || private.Precomputed.Qinv.Cmp(crt.Qinv) != 0 ||
			!slices.EqualFunc(computed, others, func(a, b [3]*big.Int) bool { return a[1].Cmp(b[1]) == 0 && a[2].Cmp(b[2]) == 0 }) {
			return nil, E.New("the JWK CRT values are inconsistent")
		}
		return private, nil
	case "ECDSA", "ECDH":
		if jwk.Kty != "EC" {
			return nil, E.New("the JWK kty is not EC")
		}
		if jwk.Crv != params.NamedCurve {
			return nil, E.New("the JWK crv is not ", params.NamedCurve)
		}
		curve, err := cryptoCurve(params.NamedCurve)
		if err != nil {
			return nil, err
		}
		size := (curve.Params().BitSize + 7) / 8
		x, y := decode("x", jwk.X), decode("y", jwk.Y)
		if failure != nil {
			return nil, failure
		}
		if len(x) != size || len(y) != size {
			return nil, E.New("the JWK coordinates are not ", size, " bytes")
		}
		point := slices.Concat([]byte{4}, x, y)
		if jwk.D == "" {
			return ecdsa.ParseUncompressedPublicKey(curve, point)
		}
		d := decode("d", jwk.D)
		if failure != nil {
			return nil, failure
		}
		if len(d) != size {
			return nil, E.New("the JWK d is not ", size, " bytes")
		}
		private, err := ecdsa.ParseRawPrivateKey(curve, d)
		if err != nil {
			return nil, err
		}
		if derived, err := private.PublicKey.Bytes(); err != nil || !bytes.Equal(derived, point) {
			return nil, E.New("the JWK public key does not match d")
		}
		return private, nil
	case "Ed25519", "X25519":
		if jwk.Kty != "OKP" {
			return nil, E.New("the JWK kty is not OKP")
		}
		if jwk.Crv != params.Name {
			return nil, E.New("the JWK crv is not ", params.Name)
		}
		x := decode("x", jwk.X)
		var d []byte
		if jwk.D != "" {
			d = decode("d", jwk.D)
		}
		if failure != nil {
			return nil, failure
		}
		if d == nil {
			return rawPublicKey(params, x)
		}
		if params.Name == "Ed25519" {
			if len(d) != ed25519.SeedSize {
				return nil, E.New("the JWK d is not ", ed25519.SeedSize, " bytes")
			}
			private := ed25519.NewKeyFromSeed(d)
			if !bytes.Equal(private.Public().(ed25519.PublicKey), x) {
				return nil, E.New("the JWK x does not match d")
			}
			return private, nil
		}
		private, err := ecdh.X25519().NewPrivateKey(d)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(private.PublicKey().Bytes(), x) {
			return nil, E.New("the JWK x does not match d")
		}
		return private, nil
	}
	return nil, E.New("unknown key pair ", params.Name)
}

func cryptoExport(_ context.Context, call *operation) (any, error) {
	var format, keyType string
	var params cryptoParams
	var material []byte
	if err := call.decode(4, &format, &params, &material, &keyType); err != nil {
		return nil, err
	}
	if format == "spki" || format == "pkcs8" {
		return jsc.Bytes(material), nil
	}
	var key any
	var err error
	if keyType == "private" {
		key, err = x509.ParsePKCS8PrivateKey(material)
	} else {
		key, err = x509.ParsePKIXPublicKey(material)
	}
	if err != nil {
		return nil, err
	}
	encode := base64.RawURLEncoding.EncodeToString
	switch k := key.(type) {
	case *rsa.PublicKey:
		return cryptoJWK{Kty: "RSA", N: encode(k.N.Bytes()), E: encode(big.NewInt(int64(k.E)).Bytes())}, nil
	case *rsa.PrivateKey:
		return cryptoJWK{
			Kty: "RSA", N: encode(k.N.Bytes()), E: encode(big.NewInt(int64(k.E)).Bytes()), D: encode(k.D.Bytes()),
			P: encode(k.Primes[0].Bytes()), Q: encode(k.Primes[1].Bytes()),
			DP: encode(k.Precomputed.Dp.Bytes()), DQ: encode(k.Precomputed.Dq.Bytes()), QI: encode(k.Precomputed.Qinv.Bytes()),
			Oth: common.Map(otherPrimes(k), func(values [3]*big.Int) cryptoPrime {
				return cryptoPrime{R: encode(values[0].Bytes()), D: encode(values[1].Bytes()), T: encode(values[2].Bytes())}
			}),
		}, nil
	case *ecdsa.PublicKey:
		point, err := k.Bytes()
		if err != nil {
			return nil, err
		}
		if format == "raw" {
			return jsc.Bytes(point), nil
		}
		size := (len(point) - 1) / 2
		return cryptoJWK{Kty: "EC", Crv: params.NamedCurve, X: encode(point[1 : 1+size]), Y: encode(point[1+size:])}, nil
	case *ecdsa.PrivateKey:
		point, err := k.PublicKey.Bytes()
		if err != nil {
			return nil, err
		}
		d, err := k.Bytes()
		if err != nil {
			return nil, err
		}
		size := len(d)
		return cryptoJWK{Kty: "EC", Crv: params.NamedCurve, X: encode(point[1 : 1+size]), Y: encode(point[1+size:]), D: encode(d)}, nil
	case ed25519.PublicKey:
		if format == "raw" {
			return jsc.Bytes(k), nil
		}
		return cryptoJWK{Kty: "OKP", Crv: "Ed25519", X: encode(k)}, nil
	case ed25519.PrivateKey:
		return cryptoJWK{Kty: "OKP", Crv: "Ed25519", X: encode(k.Public().(ed25519.PublicKey)), D: encode(k.Seed())}, nil
	case *ecdh.PublicKey:
		if format == "raw" {
			return jsc.Bytes(k.Bytes()), nil
		}
		return cryptoJWK{Kty: "OKP", Crv: "X25519", X: encode(k.Bytes())}, nil
	case *ecdh.PrivateKey:
		return cryptoJWK{Kty: "OKP", Crv: "X25519", X: encode(k.PublicKey().Bytes()), D: encode(k.Bytes())}, nil
	}
	return nil, E.New("the key cannot be exported")
}

// The encodings of Ed25519's small-order points, canonical and not (https://eprint.iacr.org/2020/1244, table 6). WebCrypto's
// Ed25519 verify returns false when the public key or the signature's R is one of them.
var ed25519SmallOrderPoints = func() [][]byte {
	encodings := []string{
		"0100000000000000000000000000000000000000000000000000000000000000",
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"0000000000000000000000000000000000000000000000000000000000000080",
		"0000000000000000000000000000000000000000000000000000000000000000",
		"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a",
		"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa",
		"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05",
		"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85",
		"0100000000000000000000000000000000000000000000000000000000000080",
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	}
	points := make([][]byte, len(encodings))
	for index, encoding := range encodings {
		points[index], _ = hex.DecodeString(encoding)
	}
	return points
}()

func ed25519SmallOrder(point []byte) bool {
	for _, small := range ed25519SmallOrderPoints {
		if bytes.Equal(point, small) {
			return true
		}
	}
	return false
}
