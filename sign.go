// sign.go - sphincs256/ref/sign.c

// Package sphincs256 implements the SPHINCS-256 practical stateless hash-based
// signature scheme.
package sphincs256

import (
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/yawning/sphincs256/hash"
	"github.com/yawning/sphincs256/horst"
	"github.com/yawning/sphincs256/utils"
	"github.com/yawning/sphincs256/wots"

	"github.com/dchest/blake512"
)

const (
	// PublicKeySize is the length of a SPHINCS-256 public key in bytes.
	PublicKeySize = (nMasks + 1) * hash.Size

	// PrivateKeySize is the length of a SPHINCS-256 private key in bytes.
	PrivateKeySize = seedBytes + PublicKeySize - hash.Size + skRandSeedBytes

	// SignatureSize is the length of a SPHINCS-256 signature in bytes.
	SignatureSize = messageHashSeedBytes + (totalTreeHeight+7)/8 + horst.SigBytes + (totalTreeHeight/subtreeHeight)*wots.SigBytes + totalTreeHeight*hash.Size

	subtreeHeight        = 5
	totalTreeHeight      = 60
	nLevels              = totalTreeHeight / subtreeHeight
	seedBytes            = 32
	skRandSeedBytes      = 32
	messageHashSeedBytes = 32
	nMasks               = 2 * horst.LogT // has to be the max of (2*(subtreeHeight+wotsLogL)) and (wotsW-1) and 2*horstLogT
)

type leafaddr struct {
	level   int
	subtree uint64
	subleaf int
}

func getSeed(seed, sk []byte, a *leafaddr) {
//	seed = seed[:seedBytes]

	var buffer [seedBytes + 8]byte
	copy(buffer[0:seedBytes], sk[0:seedBytes])

	// 4 bits to encode level.
	t := uint64(a.level)
	// 55 bits to encode subtree.
	t |= a.subtree << 4
	// 5 bits to encode leaf.
	t |= uint64(a.subleaf) << 59

	binary.LittleEndian.PutUint64(buffer[seedBytes:], t)
	hash.Varlen(seed, buffer[:])
}

func lTree(leaf, wotsPk, masks []byte) {
	l := wots.L
	for i := 0; i < wots.LogL; i++ {
		for j := 0; j < l>>1; j++ {
			hash.Hash_2n_n_mask(wotsPk[j*hash.Size:], wotsPk[j*2*hash.Size:], masks[i*2*hash.Size:])
		}

		if l&1 != 0 {
			copy(wotsPk[(l>>1)*hash.Size:((l>>1)+1)*hash.Size], wotsPk[(l-1)*hash.Size:])
			l = (l >> 1) + 1
		} else {
			l = l >> 1
		}
	}
	copy(leaf[:hash.Size], wotsPk[:])
}

func genLeafWots(leaf, masks, sk []byte, a *leafaddr) {
	var seed [seedBytes]byte
	var pk [wots.L * hash.Size]byte

	getSeed(seed[:], sk, a)
	wots.Pkgen(pk[:], seed[:], masks)
	lTree(leaf, pk[:], masks)
}

func treehash(node []byte, height int, sk []byte, leaf *leafaddr, masks []byte) {
	a := *leaf
	stack := make([]byte, (height+1)*hash.Size)
	stacklevels := make([]uint, height+1)
	var stackoffset, maskoffset uint

	lastnode := a.subleaf + (1 << uint(height))

	for ; a.subleaf < lastnode; a.subleaf++ {
		genLeafWots(stack[stackoffset*hash.Size:], masks, sk, &a)
		stacklevels[stackoffset] = 0
		stackoffset++
		for stackoffset > 1 && stacklevels[stackoffset-1] == stacklevels[stackoffset-2] {
			// Masks.
			maskoffset = 2 * (stacklevels[stackoffset-1] + wots.LogL) * hash.Size
			hash.Hash_2n_n_mask(stack[(stackoffset-2)*hash.Size:], stack[(stackoffset-2)*hash.Size:], masks[maskoffset:])
			stacklevels[stackoffset-2]++
			stackoffset--
		}
	}
	copy(node[0:hash.Size], stack[0:hash.Size])
}

func validateAuthpath(root, leaf *[hash.Size]byte, leafidx uint, authpath, masks []byte, height uint) {
	var buffer [2 * hash.Size]byte

	if leafidx&1 != 0 {
		copy(buffer[hash.Size:hash.Size*2], leaf[0:hash.Size])
		copy(buffer[0:hash.Size], authpath[0:hash.Size])
	} else {
		copy(buffer[0:hash.Size], leaf[0:hash.Size])
		copy(buffer[hash.Size:hash.Size*2], authpath[0:hash.Size])
	}
	authpath = authpath[hash.Size:]

	for i := uint(0); i < height-1; i++ {
		leafidx >>= 1
		if leafidx&1 != 0 {
			hash.Hash_2n_n_mask(buffer[hash.Size:], buffer[:], masks[2*(wots.LogL+i)*hash.Size:])
			copy(buffer[0:hash.Size], authpath[0:hash.Size])
		} else {
			hash.Hash_2n_n_mask(buffer[:], buffer[:], masks[2*(wots.LogL+i)*hash.Size:])
			copy(buffer[hash.Size:hash.Size*2], authpath[0:hash.Size])
		}
		authpath = authpath[hash.Size:]
	}
	hash.Hash_2n_n_mask(root[:], buffer[:], masks[2*(wots.LogL+height-1)*hash.Size:])
}

func computeAuthpathWots(root *[hash.Size]byte, authpath []byte, a *leafaddr, sk, masks []byte, height uint) {
	ta := *a
	var tree [2 * (1 << subtreeHeight) * hash.Size]byte
	var seed [(1 << subtreeHeight) * seedBytes]byte
	var pk [(1 << subtreeHeight) * wots.L * hash.Size]byte

	// Level 0.
	for ta.subleaf = 0; ta.subleaf < 1<<subtreeHeight; ta.subleaf++ {
		getSeed(seed[ta.subleaf*seedBytes:], sk, &ta)
	}
	for ta.subleaf = 0; ta.subleaf < 1<<subtreeHeight; ta.subleaf++ {
		wots.Pkgen(pk[ta.subleaf*wots.L*hash.Size:], seed[ta.subleaf*seedBytes:], masks)
	}
	for ta.subleaf = 0; ta.subleaf < 1<<subtreeHeight; ta.subleaf++ {
		lTree(tree[(1<<subtreeHeight)*hash.Size+ta.subleaf*hash.Size:], pk[ta.subleaf*wots.L*hash.Size:], masks)
	}

	// Tree.
	level := 0
	for i := 1 << subtreeHeight; i > 0; i >>= 1 {
		for j := 0; j < i; j += 2 {
			hash.Hash_2n_n_mask(tree[(i>>1)*hash.Size+(j>>1)*hash.Size:], tree[i*hash.Size+j*hash.Size:], masks[2*(wots.LogL+level)*hash.Size:])
		}
		level++
	}

	// Copy authpath.
	idx := a.subleaf
	for i := uint(0); i < height; i++ {
		dst := authpath[i*hash.Size : (i+1)*hash.Size]
		src := tree[((1<<subtreeHeight)>>i)*hash.Size+((idx>>i)^1)*hash.Size:]
		copy(dst[:], src[:])
	}

	// Copy root.
	copy(root[:], tree[hash.Size:])
}

// GenerateKey generates a public/private key pair using randomness from rand.
func GenerateKey(rand io.Reader) (publicKey *[PublicKeySize]byte, privateKey *[PrivateKeySize]byte, err error) {
	privateKey = new([PrivateKeySize]byte)
	publicKey = new([PublicKeySize]byte)
	_, err = io.ReadFull(rand, privateKey[:])
	if err != nil {
		return nil, nil, err
	}
	copy(publicKey[:nMasks*hash.Size], privateKey[seedBytes:])

	// Initialization of top-subtree address.
	a := leafaddr{level: nLevels - 1, subtree: 0, subleaf: 0}

	// Construct top subtree.
	treehash(publicKey[nMasks*hash.Size:], subtreeHeight, privateKey[:], &a, publicKey[:])
	return
}

// Sign signs the message with privateKey and returns the signature.
func Sign(privateKey *[PrivateKeySize]byte, message []byte) *[SignatureSize]byte {
	var sm [SignatureSize]byte
	var leafidx uint64
	var r [messageHashSeedBytes]byte
	var mH []byte
	var tsk [PrivateKeySize]byte
	var root [hash.Size]byte
	var seed [seedBytes]byte
	var masks [nMasks * hash.Size]byte

	copy(tsk[:], privateKey[:])

	// Create leafidx deterministically.
	{
		// Shift scratch upwards for convinience.
		scratch := sm[SignatureSize-skRandSeedBytes:]

		// Copy secret random seed to scratch.
		copy(scratch[:skRandSeedBytes], tsk[PrivateKeySize-skRandSeedBytes:])

		// XXX: Why Blake 512?
		h := blake512.New()
		h.Write(scratch[:skRandSeedBytes])
		h.Write(message)
		rnd := h.Sum(nil)

		// XXX/Yawning: The original code doesn't do endian conversion when
		// using rnd.  This is probably wrong, so do the Right Thing(TM).
		leafidx = binary.LittleEndian.Uint64(rnd[0:]) & 0xfffffffffffffff
		copy(r[:], rnd[16:])

		// Prepare msgHash
		scratch = sm[SignatureSize-messageHashSeedBytes-PublicKeySize:]

		// Copy R.
		copy(scratch[:], r[:])

		// Construct and copy pk.
		a := leafaddr{level: nLevels - 1, subtree: 0, subleaf: 0}
		pk := scratch[messageHashSeedBytes:]
		copy(pk[:nMasks*hash.Size], tsk[seedBytes:])
		treehash(pk[nMasks*hash.Size:], subtreeHeight, tsk[:], &a, pk)

		h.Reset()
		h.Write(scratch[:messageHashSeedBytes+PublicKeySize])
		h.Write(message)
		mH = h.Sum(nil)
	}

	// Use unique value $d$ for HORST address.
	a := leafaddr{level: nLevels, subleaf: int(leafidx & ((1 << subtreeHeight) - 1)), subtree: leafidx >> subtreeHeight}

	sigp := sm[:]

	copy(sigp[0:messageHashSeedBytes], r[:])
	sigp = sigp[messageHashSeedBytes:]

	copy(masks[:], tsk[seedBytes:])
	for i := uint64(0); i < (totalTreeHeight+7)/8; i++ {
		sigp[i] = byte((leafidx >> (8 * i)) & 0xff)
	}
	sigp = sigp[(totalTreeHeight+7)/8:]

	getSeed(seed[:], tsk[:], &a)
	horst.Sign(sigp, &root, message, &seed, masks[:], mH)
	sigp = sigp[horst.SigBytes:]

	for i := 0; i < nLevels; i++ {
		a.level = i

		getSeed(seed[:], tsk[:], &a) // XXX: Don't use the same address as for horst_sign here!
		wots.Sign(sigp, &root, &seed, masks[:])
		sigp = sigp[wots.SigBytes:]

		computeAuthpathWots(&root, sigp, &a, tsk[:], masks[:], subtreeHeight)
		sigp = sigp[subtreeHeight*hash.Size:]

		a.subleaf = int(a.subtree & ((1 << subtreeHeight) - 1))
		a.subtree >>= subtreeHeight
	}

	utils.Zerobytes(tsk[:])

	return &sm
}

// Verify takes a public key, message and signature and returns true if the
// signature is valid.
func Verify(publicKey *[PublicKeySize]byte, message []byte, signature *[SignatureSize]byte) bool {
	var leafidx uint64
	var wotsPk [wots.L * hash.Size]byte
	var pkhash [hash.Size]byte
	var root [hash.Size]byte
	var tpk [PublicKeySize]byte
	var mH []byte

	copy(tpk[:], publicKey[:])

	// Construct message hash.
	h := blake512.New()
	h.Write(signature[:messageHashSeedBytes])
	h.Write(tpk[:])
	h.Write(message)
	mH = h.Sum(nil)

	sigp := signature[:]
	sigp = sigp[messageHashSeedBytes:]
	for i := uint64(0); i < (totalTreeHeight+7)/8; i++ {
		leafidx |= uint64(sigp[i]) << (8 * i)
	}

	// XXX/Yawning: Check the return value?
	horst.Verify(root[:], sigp[(totalTreeHeight+7)/8:], sigp[SignatureSize-messageHashSeedBytes:], tpk[:], mH[:])

	sigp = sigp[(totalTreeHeight+7)/8:]
	sigp = sigp[horst.SigBytes:]

	for i := 0; i < nLevels; i++ {
		wots.Verify(&wotsPk, sigp, &root, tpk[:])
		sigp = sigp[wots.SigBytes:]

		lTree(pkhash[:], wotsPk[:], tpk[:])
		validateAuthpath(&root, &pkhash, uint(leafidx&0x1f), sigp, tpk[:], subtreeHeight)
		leafidx >>= 5
		sigp = sigp[subtreeHeight*hash.Size:]
	}

	tpkRewt := tpk[nMasks*hash.Size:]
	return subtle.ConstantTimeCompare(root[:], tpkRewt) == 1
}

// Open takes a signed message and public key and returns the message if the
// signature is valid.
func Open(publicKey *[PublicKeySize]byte, message []byte) (body []byte, err error) {
	if len(message) < SignatureSize {
		return nil, fmt.Errorf("sphincs256: message length is too short to be valid")
	}

	var sig [SignatureSize]byte
	copy(sig[:], message[:SignatureSize])
	body = message[SignatureSize:]

	if Verify(publicKey, body, &sig) == false {
		return nil, fmt.Errorf("sphics256: signature verification failed")
	}
	return body, nil
}

func init() {
	// Note: Since I split horst and wots into their own packages, validate
	// that SeedBytes is consistent.
	if horst.SeedBytes != seedBytes || wots.SeedBytes != seedBytes {
		panic("SEED_BYTES must equal horst.SeedBytes and wots.SeedBytes")
	}

	if totalTreeHeight-subtreeHeight > 64 {
		panic("TOTALTREE_HEIGHT-SUBTREE_HEIGHT must be at most 64")
	}
	if nLevels > 15 || nLevels < 8 {
		// XXX/Yawning: The original code's compile time check for this
		// invariant is broken.
		panic("need to have 8 <= N_LEVELS <= 15")
	}
	if subtreeHeight != 5 {
		panic("need to have SUBTREE_HEIGHT == 5")
	}
	if totalTreeHeight != 60 {
		panic("need to have TOTALTREE_HEIGHT == 60")
	}
	if seedBytes != hash.Size {
		panic("need to have SEED_BYTES == HASH_BYTES")
	}
	if messageHashSeedBytes != 32 {
		panic("need to have MESSAGE_HASH_SEED_BYTES == 32")
	}
}
