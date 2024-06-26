// This package is a modified version of the "gcm" implementation from the cipher package included in go.
// It adds support for Apple's use of null initialization vectors in iOS backup keychains.

// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gcm

import (
	"crypto/cipher"
	"crypto/subtle"
	"errors"
)

// gcmFieldElement represents a value in GF(2¹²⁸). In order to reflect the GCM
// standard and make getUint64 suitable for marshaling these values, the bits
// are stored backwards. For example:
//   the coefficient of x⁰ can be obtained by v.low >> 63.
//   the coefficient of x⁶³ can be obtained by v.low & 1.
//   the coefficient of x⁶⁴ can be obtained by v.high >> 63.
//   the coefficient of x¹²⁷ can be obtained by v.high & 1.
type gcmFieldElement struct {
	low, high uint64
}

// gcm represents a Galois Counter Mode with a specific key. See
// http://csrc.nist.gov/groups/ST/toolkit/BCM/documents/proposedmodes/gcm/gcm-revised-spec.pdf
type gcm struct {
	cipher cipher.Block
	// productTable contains the first sixteen powers of the key, H.
	// However, they are in bit reversed order. See NewGCM.
	productTable [16]gcmFieldElement
}

// NewGCM returns the given 128-bit, block cipher wrapped in Galois Counter Mode.
func NewGCM(cipher cipher.Block) (cipher.AEAD, error) {
	if cipher.BlockSize() != gcmBlockSize {
		return nil, errors.New("cipher: NewGCM requires 128-bit block cipher")
	}

	var key [gcmBlockSize]byte
	cipher.Encrypt(key[:], key[:])

	g := &gcm{cipher: cipher}

	// We precompute 16 multiples of |key|. However, when we do lookups
	// into this table we'll be using bits from a field element and
	// therefore the bits will be in the reverse order. So normally one
	// would expect, say, 4*key to be in index 4 of the table but due to
	// this bit ordering it will actually be in index 0010 (base 2) = 2.
	x := gcmFieldElement{
		getUint64(key[:8]),
		getUint64(key[8:]),
	}
	g.productTable[reverseBits(1)] = x

	for i := 2; i < 16; i += 2 {
		g.productTable[reverseBits(i)] = gcmDouble(&g.productTable[reverseBits(i/2)])
		g.productTable[reverseBits(i+1)] = gcmAdd(&g.productTable[reverseBits(i)], &x)
	}

	return g, nil
}

const (
	gcmBlockSize = 16
	gcmTagSize   = 16
	gcmNonceSize = 12
)

func (*gcm) NonceSize() int {
	return gcmNonceSize
}

func (*gcm) Overhead() int {
	return gcmTagSize
}

func (g *gcm) Seal(dst, nonce, plaintext, data []byte) []byte {
	ret, out := sliceForAppend(dst, len(plaintext)+gcmTagSize)

	// See GCM spec, section 7.1.
	var counter, tagMask [gcmBlockSize]byte
	if len(nonce) != gcmNonceSize {
		// counter is all zeros for apple's blank iv
		// The python code seems to do the hash inside g.auth for non-12 byte IVs
	} else {
		copy(counter[:], nonce)
		counter[gcmBlockSize-1] = 1
	}

	g.cipher.Encrypt(tagMask[:], counter[:])
	gcmInc32(&counter)

	g.counterCrypt(out, plaintext, &counter)
	g.auth(out[len(plaintext):], out[:len(plaintext)], data, &tagMask)

	return ret
}

var errOpen = errors.New("cipher: message authentication failed")

func (g *gcm) Open(dst, nonce, ciphertext, data []byte) ([]byte, error) {

	if len(ciphertext) < gcmTagSize {
		return nil, errOpen
	}
	tag := ciphertext[len(ciphertext)-gcmTagSize:]
	ciphertext = ciphertext[:len(ciphertext)-gcmTagSize]

	// See GCM spec, section 7.1.
	var counter, tagMask [gcmBlockSize]byte

	if len(nonce) != gcmNonceSize {
		// counter is all zeros for apple's blank iv
		// The python code seems to do the hash inside g.auth for non-12 byte IVs

	} else {
		copy(counter[:], nonce)
		counter[gcmBlockSize-1] = 1
	}

	g.cipher.Encrypt(tagMask[:], counter[:])
	gcmInc32(&counter)
	var expectedTag [gcmTagSize]byte
	g.auth(expectedTag[:], ciphertext, data, &tagMask)

	if subtle.ConstantTimeCompare(expectedTag[:], tag) != 1 {
		return nil, errOpen
	}

	ret, out := sliceForAppend(dst, len(ciphertext))
	g.counterCrypt(out, ciphertext, &counter)

	return ret, nil
}

// reverseBits reverses the order of the bits of 4-bit number in i.
func reverseBits(i int) int {
	i = ((i << 2) & 0xc) | ((i >> 2) & 0x3)
	i = ((i << 1) & 0xa) | ((i >> 1) & 0x5)
	return i
}

// gcmAdd adds two elements of GF(2¹²⁸) and returns the sum.
func gcmAdd(x, y *gcmFieldElement) gcmFieldElement {
	// Addition in a characteristic 2 field is just XOR.
	return gcmFieldElement{x.low ^ y.low, x.high ^ y.high}
}

// gcmDouble returns the result of doubling an element of GF(2¹²⁸).
func gcmDouble(x *gcmFieldElement) (double gcmFieldElement) {
	msbSet := x.high&1 == 1

	// Because of the bit-ordering, doubling is actually a right shift.
	double.high = x.high >> 1
	double.high |= x.low << 63
	double.low = x.low >> 1

	// If the most-significant bit was set before shifting then it,
	// conceptually, becomes a term of x^128. This is greater than the
	// irreducible polynomial so the result has to be reduced. The
	// irreducible polynomial is 1+x+x^2+x^7+x^128. We can subtract that to
	// eliminate the term at x^128 which also means subtracting the other
	// four terms. In characteristic 2 fields, subtraction == addition ==
	// XOR.
	if msbSet {
		double.low ^= 0xe100000000000000
	}

	return
}

var gcmReductionTable = []uint16{
	0x0000, 0x1c20, 0x3840, 0x2460, 0x7080, 0x6ca0, 0x48c0, 0x54e0,
	0xe100, 0xfd20, 0xd940, 0xc560, 0x9180, 0x8da0, 0xa9c0, 0xb5e0,
}

// mul sets y to y*H, where H is the GCM key, fixed during NewGCM.
func (g *gcm) mul(y *gcmFieldElement) {
	var z gcmFieldElement

	for i := 0; i < 2; i++ {
		word := y.high
		if i == 1 {
			word = y.low
		}

		// Multiplication works by multiplying z by 16 and adding in
		// one of the precomputed multiples of H.
		for j := 0; j < 64; j += 4 {
			msw := z.high & 0xf
			z.high >>= 4
			z.high |= z.low << 60
			z.low >>= 4
			z.low ^= uint64(gcmReductionTable[msw]) << 48

			// the values in |table| are ordered for
			// little-endian bit positions. See the comment
			// in NewGCM.
			t := &g.productTable[word&0xf]

			z.low ^= t.low
			z.high ^= t.high
			word >>= 4
		}
	}

	*y = z
}

// updateBlocks extends y with more polynomial terms from blocks, based on
// Horner's rule. There must be a multiple of gcmBlockSize bytes in blocks.
func (g *gcm) updateBlocks(y *gcmFieldElement, blocks []byte) {
	for len(blocks) > 0 {
		y.low ^= getUint64(blocks)
		y.high ^= getUint64(blocks[8:])
		g.mul(y)
		blocks = blocks[gcmBlockSize:]
	}
}

// update extends y with more polynomial terms from data. If data is not a
// multiple of gcmBlockSize bytes long then the remainder is zero padded.
func (g *gcm) update(y *gcmFieldElement, data []byte) {
	fullBlocks := (len(data) >> 4) << 4
	g.updateBlocks(y, data[:fullBlocks])

	if len(data) != fullBlocks {
		var partialBlock [gcmBlockSize]byte
		copy(partialBlock[:], data[fullBlocks:])
		g.updateBlocks(y, partialBlock[:])
	}
}

// gcmInc32 treats the final four bytes of counterBlock as a big-endian value
// and increments it.
func gcmInc32(counterBlock *[16]byte) {
	for i := gcmBlockSize - 1; i >= gcmBlockSize-4; i-- {
		counterBlock[i]++
		if counterBlock[i] != 0 {
			break
		}
	}
}

// sliceForAppend takes a slice and a requested number of bytes. It returns a
// slice with the contents of the given slice followed by that many bytes and a
// second slice that aliases into it and contains only the extra bytes. If the
// original slice has sufficient capacity then no allocation is performed.
func sliceForAppend(in []byte, n int) (head, tail []byte) {
	if total := len(in) + n; cap(in) >= total {
		head = in[:total]
	} else {
		head = make([]byte, total)
		copy(head, in)
	}
	tail = head[len(in):]
	return
}

// counterCrypt crypts in to out using g.cipher in counter mode.
func (g *gcm) counterCrypt(out, in []byte, counter *[gcmBlockSize]byte) {
	var mask [gcmBlockSize]byte

	for len(in) >= gcmBlockSize {
		g.cipher.Encrypt(mask[:], counter[:])
		gcmInc32(counter)

		xorWords(out, in, mask[:])
		out = out[gcmBlockSize:]
		in = in[gcmBlockSize:]
	}

	if len(in) > 0 {
		g.cipher.Encrypt(mask[:], counter[:])
		gcmInc32(counter)
		xorBytes(out, in, mask[:])
	}
}

// auth calculates GHASH(ciphertext, additionalData), masks the result with
// tagMask and writes the result to out.
func (g *gcm) auth(out, ciphertext, additionalData []byte, tagMask *[gcmTagSize]byte) {
	var y gcmFieldElement
	g.update(&y, additionalData)
	g.update(&y, ciphertext)

	y.low ^= uint64(len(additionalData)) * 8
	y.high ^= uint64(len(ciphertext)) * 8

	g.mul(&y)

	putUint64(out, y.low)
	putUint64(out[8:], y.high)

	xorWords(out, out, tagMask[:])
}

func getUint64(data []byte) uint64 {
	r := uint64(data[0])<<56 |
		uint64(data[1])<<48 |
		uint64(data[2])<<40 |
		uint64(data[3])<<32 |
		uint64(data[4])<<24 |
		uint64(data[5])<<16 |
		uint64(data[6])<<8 |
		uint64(data[7])
	return r
}

func putUint64(out []byte, v uint64) {
	out[0] = byte(v >> 56)
	out[1] = byte(v >> 48)
	out[2] = byte(v >> 40)
	out[3] = byte(v >> 32)
	out[4] = byte(v >> 24)
	out[5] = byte(v >> 16)
	out[6] = byte(v >> 8)
	out[7] = byte(v)
}
