package libtrust

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
)

// PublicKey is a generic interface for a Public Key.
type PublicKey interface {
	// KeyType returns the key type for this key. For elliptic curve keys,
	// this value should be "EC". For RSA keys, this value should be "RSA".
	KeyType() string
	// KeyID returns a distinct identifier which is unique to this Public Key.
	// The format generated by this library is a base32 encoding of a 240 bit
	// hash of the public key data divided into 12 groups like so:
	//    ABCD:EFGH:IJKL:MNOP:QRST:UVWX:YZ23:4567:ABCD:EFGH:IJKL:MNOP
	KeyID() string
	// Verify verifyies the signature of the data in the io.Reader using this
	// Public Key. The alg parameter should identify the digital signature
	// algorithm which was used to produce the signature and should be
	// supported by this public key. Returns a nil error if the signature
	// is valid.
	Verify(data io.Reader, alg string, signature []byte) error
	// CryptoPublicKey returns the internal object which can be used as a
	// crypto.PublicKey for use with other standard library operations. The type
	// is either *rsa.PublicKey or *ecdsa.PublicKey
	CryptoPublicKey() crypto.PublicKey
	// These public keys can be serialized to the standard JSON encoding for
	// JSON Web Keys. See section 6 of the IETF draft RFC for JOSE JSON Web
	// Algorithms.
	MarshalJSON() ([]byte, error)
	// These keys can also be serialized to the standard PEM encoding.
	PEMBlock() (*pem.Block, error)
	// The string representation of a key is its key type and ID.
	String() string
	AddExtendedField(string, interface{})
	GetExtendedField(string) interface{}
}

// PrivateKey is a generic interface for a Private Key.
type PrivateKey interface {
	// A PrivateKey contains all fields and methods of a PublicKey of the
	// same type. The MarshalJSON method also outputs the private key as a
	// JSON Web Key, and the PEMBlock method outputs the private key as a
	// PEM block.
	PublicKey
	// PublicKey returns the PublicKey associated with this PrivateKey.
	PublicKey() PublicKey
	// Sign signs the data read from the io.Reader using a signature algorithm
	// supported by the private key. If the specified hashing algorithm is
	// supported by this key, that hash function is used to generate the
	// signature otherwise the default hashing algorithm for this key is
	// used. Returns the signature and identifier of the algorithm used.
	Sign(data io.Reader, hashID crypto.Hash) (signature []byte, alg string, err error)
	// CryptoPrivateKey returns the internal object which can be used as a
	// crypto.PublicKey for use with other standard library operations. The
	// type is either *rsa.PublicKey or *ecdsa.PublicKey
	CryptoPrivateKey() crypto.PrivateKey
}

// FromCryptoPublicKey returns a libtrust PublicKey representation of the given
// *ecdsa.PublicKey or *rsa.PublicKey. Returns a non-nil error when the given
// key is of an unsupported type.
func FromCryptoPublicKey(cryptoPublicKey crypto.PublicKey) (PublicKey, error) {
	switch cryptoPublicKey := cryptoPublicKey.(type) {
	case *ecdsa.PublicKey:
		return fromECPublicKey(cryptoPublicKey)
	case *rsa.PublicKey:
		return fromRSAPublicKey(cryptoPublicKey), nil
	default:
		return nil, fmt.Errorf("public key type %T is not supported", cryptoPublicKey)
	}
}

// FromCryptoPrivateKey returns a libtrust PrivateKey representation of the given
// *ecdsa.PrivateKey or *rsa.PrivateKey. Returns a non-nil error when the given
// key is of an unsupported type.
func FromCryptoPrivateKey(cryptoPrivateKey crypto.PrivateKey) (PrivateKey, error) {
	switch cryptoPrivateKey := cryptoPrivateKey.(type) {
	case *ecdsa.PrivateKey:
		return fromECPrivateKey(cryptoPrivateKey)
	case *rsa.PrivateKey:
		return fromRSAPrivateKey(cryptoPrivateKey), nil
	default:
		return nil, fmt.Errorf("private key type %T is not supported", cryptoPrivateKey)
	}
}

// UnmarshalPublicKeyPEM parses the PEM encoded data and returns a libtrust
// PublicKey or an error if there is a problem with the encoding.
func UnmarshalPublicKeyPEM(data []byte) (PublicKey, error) {
	pemBlock, _ := pem.Decode(data)
	if pemBlock == nil {
		return nil, errors.New("unable to find PEM encoded data")
	} else if pemBlock.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("unable to get PublicKey from PEM type: %s", pemBlock.Type)
	}

	return pubKeyFromPEMBlock(pemBlock)
}

// UnmarshalPublicKeyPEMBundle parses the PEM encoded data as a bundle of
// PEM blocks appended one after the other and returns a slice of PublicKey
// objects that it finds.
func UnmarshalPublicKeyPEMBundle(data []byte) ([]PublicKey, error) {
	pubKeys := []PublicKey{}

	for {
		var pemBlock *pem.Block
		pemBlock, data = pem.Decode(data)
		if pemBlock == nil {
			break
		} else if pemBlock.Type != "PUBLIC KEY" {
			return nil, fmt.Errorf("unable to get PublicKey from PEM type: %s", pemBlock.Type)
		}

		pubKey, err := pubKeyFromPEMBlock(pemBlock)
		if err != nil {
			return nil, err
		}

		pubKeys = append(pubKeys, pubKey)
	}

	return pubKeys, nil
}

// UnmarshalPrivateKeyPEM parses the PEM encoded data and returns a libtrust
// PrivateKey or an error if there is a problem with the encoding.
func UnmarshalPrivateKeyPEM(data []byte) (PrivateKey, error) {
	pemBlock, _ := pem.Decode(data)
	if pemBlock == nil {
		return nil, errors.New("unable to find PEM encoded data")
	}

	var key PrivateKey

	switch {
	case pemBlock.Type == "RSA PRIVATE KEY":
		rsaPrivateKey, err := x509.ParsePKCS1PrivateKey(pemBlock.Bytes)
		if err != nil {
			return nil, fmt.Errorf("unable to decode RSA Private Key PEM data: %s", err)
		}
		key = fromRSAPrivateKey(rsaPrivateKey)
	case pemBlock.Type == "EC PRIVATE KEY":
		ecPrivateKey, err := x509.ParseECPrivateKey(pemBlock.Bytes)
		if err != nil {
			return nil, fmt.Errorf("unable to decode EC Private Key PEM data: %s", err)
		}
		key, err = fromECPrivateKey(ecPrivateKey)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unable to get PrivateKey from PEM type: %s", pemBlock.Type)
	}

	addPEMHeadersToKey(pemBlock, key.PublicKey())

	return key, nil
}

// UnmarshalPublicKeyJWK unmarshals the given JSON Web Key into a generic
// Public Key to be used with libtrust.
func UnmarshalPublicKeyJWK(data []byte) (PublicKey, error) {
	jwk := make(map[string]interface{})

	err := json.Unmarshal(data, &jwk)
	if err != nil {
		return nil, fmt.Errorf(
			"decoding JWK Public Key JSON data: %s\n", err,
		)
	}

	// Get the Key Type value.
	kty, err := stringFromMap(jwk, "kty")
	if err != nil {
		return nil, fmt.Errorf("JWK Public Key type: %s", err)
	}

	switch {
	case kty == "EC":
		// Call out to unmarshal EC public key.
		return ecPublicKeyFromMap(jwk)
	case kty == "RSA":
		// Call out to unmarshal RSA public key.
		return rsaPublicKeyFromMap(jwk)
	default:
		return nil, fmt.Errorf(
			"JWK Public Key type not supported: %q\n", kty,
		)
	}
}

// UnmarshalPublicKeyJWKSet parses the JSON encoded data as a JSON Web Key Set
// and returns a slice of Public Key objects.
func UnmarshalPublicKeyJWKSet(data []byte) ([]PublicKey, error) {
	rawKeys, err := loadJSONKeySetRaw(data)
	if err != nil {
		return nil, err
	}

	pubKeys := make([]PublicKey, 0, len(rawKeys))

	for _, rawKey := range rawKeys {
		pubKey, err := UnmarshalPublicKeyJWK(rawKey)
		if err != nil {
			return nil, err
		}
		pubKeys = append(pubKeys, pubKey)
	}

	return pubKeys, nil
}

// UnmarshalPrivateKeyJWK unmarshals the given JSON Web Key into a generic
// Private Key to be used with libtrust.
func UnmarshalPrivateKeyJWK(data []byte) (PrivateKey, error) {
	jwk := make(map[string]interface{})

	err := json.Unmarshal(data, &jwk)
	if err != nil {
		return nil, fmt.Errorf(
			"decoding JWK Private Key JSON data: %s\n", err,
		)
	}

	// Get the Key Type value.
	kty, err := stringFromMap(jwk, "kty")
	if err != nil {
		return nil, fmt.Errorf("JWK Private Key type: %s", err)
	}

	switch {
	case kty == "EC":
		// Call out to unmarshal EC private key.
		return ecPrivateKeyFromMap(jwk)
	case kty == "RSA":
		// Call out to unmarshal RSA private key.
		return rsaPrivateKeyFromMap(jwk)
	default:
		return nil, fmt.Errorf(
			"JWK Private Key type not supported: %q\n", kty,
		)
	}
}
