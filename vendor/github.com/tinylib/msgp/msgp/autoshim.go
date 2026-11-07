package msgp

import "strconv"

// AutoShim provides helper functions for converting between string and
// numeric types.
type AutoShim struct{}

// ParseUint converts a string to a uint.
func (a AutoShim) ParseUint(s string) (uint, error) {
	v, err := strconv.ParseUint(s, 10, strconv.IntSize)
	return uint(v), err
}

// ParseUint8 converts a string to a uint8.
func (a AutoShim) ParseUint8(s string) (uint8, error) {
	v, err := strconv.ParseUint(s, 10, 8)
	return uint8(v), err
}

// ParseUint16 converts a string to a uint16.
func (a AutoShim) ParseUint16(s string) (uint16, error) {
	v, err := strconv.ParseUint(s, 10, 16)
	return uint16(v), err
}

// ParseUint32 converts a string to a uint32.
func (a AutoShim) ParseUint32(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	return uint32(v), err
}

// ParseUint64 converts a string to a uint64.
func (a AutoShim) ParseUint64(s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	return uint64(v), err
}

// ParseInt converts a string to an int.
func (a AutoShim) ParseInt(s string) (int, error) {
	v, err := strconv.ParseInt(s, 10, strconv.IntSize)
	return int(v), err
}

// ParseInt8 converts a string to an int8.
func (a AutoShim) ParseInt8(s string) (int8, error) {
	v, err := strconv.ParseInt(s, 10, 8)
	return int8(v), err
}

// ParseInt16 converts a string to an int16.
func (a AutoShim) ParseInt16(s string) (int16, error) {
	v, err := strconv.ParseInt(s, 10, 16)
	return int16(v), err
}

// ParseInt32 converts a string to an int32.
func (a AutoShim) ParseInt32(s string) (int32, error) {
	v, err := strconv.ParseInt(s, 10, 32)
	return int32(v), err
}

// ParseInt64 converts a string to an int64.
func (a AutoShim) ParseInt64(s string) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	return int64(v), err
}

// ParseBool converts a string to a bool.
func (a AutoShim) ParseBool(s string) (bool, error) {
	return strconv.ParseBool(s)
}

// ParseFloat64 converts a string to a float64.
func (a AutoShim) ParseFloat64(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

// ParseFloat32 converts a string to a float32.
func (a AutoShim) ParseFloat32(s string) (float32, error) {
	v, err := strconv.ParseFloat(s, 32)
	return float32(v), err
}

// ParseByte converts a string to a byte.
func (a AutoShim) ParseByte(s string) (byte, error) {
	v, err := strconv.ParseUint(s, 10, 8)
	return byte(v), err
}

// Uint8String returns the string representation of a uint8.
func (a AutoShim) Uint8String(v uint8) string {
	return strconv.FormatUint(uint64(v), 10)
}

// UintString returns the string representation of a uint.
func (a AutoShim) UintString(v uint) string {
	return strconv.FormatUint(uint64(v), 10)
}

// Uint16String returns the string representation of a uint16.
func (a AutoShim) Uint16String(v uint16) string {
	return strconv.FormatUint(uint64(v), 10)
}

// Uint32String returns the string representation of a uint32.
func (a AutoShim) Uint32String(v uint32) string {
	return strconv.FormatUint(uint64(v), 10)
}

// Uint64String returns the string representation of a uint64.
func (a AutoShim) Uint64String(v uint64) string {
	return strconv.FormatUint(v, 10)
}

// IntString returns the string representation of an int.
func (a AutoShim) IntString(v int) string {
	return strconv.FormatInt(int64(v), 10)
}

// Int8String returns the string representation of an int8.
func (a AutoShim) Int8String(v int8) string {
	return strconv.FormatInt(int64(v), 10)
}

// Int16String returns the string representation of an int16.
func (a AutoShim) Int16String(v int16) string {
	return strconv.FormatInt(int64(v), 10)
}

// Int32String returns the string representation of an int32.
func (a AutoShim) Int32String(v int32) string {
	return strconv.FormatInt(int64(v), 10)
}

// Int64String returns the string representation of an int64.
func (a AutoShim) Int64String(v int64) string {
	return strconv.FormatInt(v, 10)
}

// BoolString returns the string representation of a bool.
func (a AutoShim) BoolString(v bool) string {
	return strconv.FormatBool(v)
}

// Float64String returns the string representation of a float64.
func (a AutoShim) Float64String(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Float32String returns the string representation of a float32.
func (a AutoShim) Float32String(v float32) string {
	return strconv.FormatFloat(float64(v), 'g', -1, 32)
}

// ByteString returns the string representation of a byte.
func (a AutoShim) ByteString(v byte) string {
	return strconv.FormatUint(uint64(v), 10)
}
