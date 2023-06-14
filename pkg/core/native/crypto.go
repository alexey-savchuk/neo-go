package native

import (
	"crypto/elliptic"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	bls12381 "github.com/consensys/gnark-crypto/ecc/bls12-381"
	"github.com/consensys/gnark-crypto/ecc/bls12-381/fr"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/nspcc-dev/neo-go/pkg/core/dao"
	"github.com/nspcc-dev/neo-go/pkg/core/interop"
	"github.com/nspcc-dev/neo-go/pkg/core/native/nativenames"
	"github.com/nspcc-dev/neo-go/pkg/crypto/hash"
	"github.com/nspcc-dev/neo-go/pkg/crypto/keys"
	"github.com/nspcc-dev/neo-go/pkg/smartcontract"
	"github.com/nspcc-dev/neo-go/pkg/smartcontract/callflag"
	"github.com/nspcc-dev/neo-go/pkg/smartcontract/manifest"
	"github.com/nspcc-dev/neo-go/pkg/vm/stackitem"
	"github.com/twmb/murmur3"
)

// Crypto represents CryptoLib contract.
type Crypto struct {
	interop.ContractMD
}

// NamedCurve identifies named elliptic curves.
type NamedCurve byte

// Various named elliptic curves.
const (
	Secp256k1 NamedCurve = 22
	Secp256r1 NamedCurve = 23
)

const cryptoContractID = -3

func newCrypto() *Crypto {
	c := &Crypto{ContractMD: *interop.NewContractMD(nativenames.CryptoLib, cryptoContractID)}
	defer c.UpdateHash()

	desc := newDescriptor("sha256", smartcontract.ByteArrayType,
		manifest.NewParameter("data", smartcontract.ByteArrayType))
	md := newMethodAndPrice(c.sha256, 1<<15, callflag.NoneFlag)
	c.AddMethod(md, desc)

	desc = newDescriptor("ripemd160", smartcontract.ByteArrayType,
		manifest.NewParameter("data", smartcontract.ByteArrayType))
	md = newMethodAndPrice(c.ripemd160, 1<<15, callflag.NoneFlag)
	c.AddMethod(md, desc)

	desc = newDescriptor("murmur32", smartcontract.ByteArrayType,
		manifest.NewParameter("data", smartcontract.ByteArrayType),
		manifest.NewParameter("seed", smartcontract.IntegerType))
	md = newMethodAndPrice(c.murmur32, 1<<13, callflag.NoneFlag)
	c.AddMethod(md, desc)

	desc = newDescriptor("verifyWithECDsa", smartcontract.BoolType,
		manifest.NewParameter("message", smartcontract.ByteArrayType),
		manifest.NewParameter("pubkey", smartcontract.ByteArrayType),
		manifest.NewParameter("signature", smartcontract.ByteArrayType),
		manifest.NewParameter("curve", smartcontract.IntegerType))
	md = newMethodAndPrice(c.verifyWithECDsa, 1<<15, callflag.NoneFlag)
	c.AddMethod(md, desc)

	desc = newDescriptor("bls12381Serialize", smartcontract.ByteArrayType,
		manifest.NewParameter("g", smartcontract.InteropInterfaceType))
	md = newMethodAndPrice(c.bls12381Serialize, 1<<19, callflag.NoneFlag)
	c.AddMethod(md, desc)

	desc = newDescriptor("bls12381Deserialize", smartcontract.InteropInterfaceType,
		manifest.NewParameter("data", smartcontract.ByteArrayType))
	md = newMethodAndPrice(c.bls12381Deserialize, 1<<19, callflag.NoneFlag)
	c.AddMethod(md, desc)

	desc = newDescriptor("bls12381Equal", smartcontract.BoolType,
		manifest.NewParameter("x", smartcontract.InteropInterfaceType),
		manifest.NewParameter("y", smartcontract.InteropInterfaceType))
	md = newMethodAndPrice(c.bls12381Equal, 1<<5, callflag.NoneFlag)
	c.AddMethod(md, desc)

	desc = newDescriptor("bls12381Add", smartcontract.InteropInterfaceType,
		manifest.NewParameter("x", smartcontract.InteropInterfaceType),
		manifest.NewParameter("y", smartcontract.InteropInterfaceType))
	md = newMethodAndPrice(c.bls12381Add, 1<<19, callflag.NoneFlag)
	c.AddMethod(md, desc)

	desc = newDescriptor("bls12381Mul", smartcontract.InteropInterfaceType,
		manifest.NewParameter("x", smartcontract.InteropInterfaceType),
		manifest.NewParameter("mul", smartcontract.ByteArrayType),
		manifest.NewParameter("neg", smartcontract.BoolType))
	md = newMethodAndPrice(c.bls12381Mul, 1<<21, callflag.NoneFlag)
	c.AddMethod(md, desc)

	desc = newDescriptor("bls12381Pairing", smartcontract.InteropInterfaceType,
		manifest.NewParameter("g1", smartcontract.InteropInterfaceType),
		manifest.NewParameter("g2", smartcontract.InteropInterfaceType))
	md = newMethodAndPrice(c.bls12381Pairing, 1<<23, callflag.NoneFlag)
	c.AddMethod(md, desc)

	return c
}

func (c *Crypto) sha256(_ *interop.Context, args []stackitem.Item) stackitem.Item {
	bs, err := args[0].TryBytes()
	if err != nil {
		panic(err)
	}
	return stackitem.NewByteArray(hash.Sha256(bs).BytesBE())
}

func (c *Crypto) ripemd160(_ *interop.Context, args []stackitem.Item) stackitem.Item {
	bs, err := args[0].TryBytes()
	if err != nil {
		panic(err)
	}
	return stackitem.NewByteArray(hash.RipeMD160(bs).BytesBE())
}

func (c *Crypto) murmur32(_ *interop.Context, args []stackitem.Item) stackitem.Item {
	bs, err := args[0].TryBytes()
	if err != nil {
		panic(err)
	}
	seed := toUint32(args[1])
	h := murmur3.SeedSum32(seed, bs)
	result := make([]byte, 4)
	binary.LittleEndian.PutUint32(result, h)
	return stackitem.NewByteArray(result)
}

func (c *Crypto) verifyWithECDsa(_ *interop.Context, args []stackitem.Item) stackitem.Item {
	msg, err := args[0].TryBytes()
	if err != nil {
		panic(fmt.Errorf("invalid message stackitem: %w", err))
	}
	hashToCheck := hash.Sha256(msg)
	pubkey, err := args[1].TryBytes()
	if err != nil {
		panic(fmt.Errorf("invalid pubkey stackitem: %w", err))
	}
	signature, err := args[2].TryBytes()
	if err != nil {
		panic(fmt.Errorf("invalid signature stackitem: %w", err))
	}
	curve, err := curveFromStackitem(args[3])
	if err != nil {
		panic(fmt.Errorf("invalid curve stackitem: %w", err))
	}
	pkey, err := keys.NewPublicKeyFromBytes(pubkey, curve)
	if err != nil {
		panic(fmt.Errorf("failed to decode pubkey: %w", err))
	}
	res := pkey.Verify(signature, hashToCheck.BytesBE())
	return stackitem.NewBool(res)
}

func curveFromStackitem(si stackitem.Item) (elliptic.Curve, error) {
	curve, err := si.TryInteger()
	if err != nil {
		return nil, err
	}
	if !curve.IsInt64() {
		return nil, errors.New("not an int64")
	}
	c := curve.Int64()
	switch c {
	case int64(Secp256k1):
		return secp256k1.S256(), nil
	case int64(Secp256r1):
		return elliptic.P256(), nil
	default:
		return nil, errors.New("unsupported curve type")
	}
}

func (c *Crypto) bls12381Serialize(_ *interop.Context, args []stackitem.Item) stackitem.Item {
	val, ok := args[0].(*stackitem.Interop).Value().(blsPoint)
	if !ok {
		panic(errors.New("not a bls12381 point"))
	}
	return stackitem.NewByteArray(val.Bytes())
}

func (c *Crypto) bls12381Deserialize(_ *interop.Context, args []stackitem.Item) stackitem.Item {
	buf, err := args[0].TryBytes()
	if err != nil {
		panic(fmt.Errorf("invalid serialized bls12381 point: %w", err))
	}
	var res interface{}
	switch l := len(buf); l {
	case bls12381.SizeOfG1AffineCompressed:
		g1Affine := new(bls12381.G1Affine)
		_, err = g1Affine.SetBytes(buf)
		if err != nil {
			panic(fmt.Errorf("failed to decode bls12381 G1Affine point: %w", err))
		}
		res = g1Affine
	case bls12381.SizeOfG2AffineCompressed:
		g2Affine := new(bls12381.G2Affine)
		_, err = g2Affine.SetBytes(buf)
		if err != nil {
			panic(fmt.Errorf("failed to decode bls12381 G2Affine point: %w", err))
		}
		res = g2Affine
	case bls12381.SizeOfGT:
		gt := new(bls12381.GT)
		err := gt.SetBytes(buf)
		if err != nil {
			panic(fmt.Errorf("failed to decode GT point: %w", err))
		}
		res = gt
	}
	return stackitem.NewInterop(blsPoint{point: res})
}

func (c *Crypto) bls12381Equal(_ *interop.Context, args []stackitem.Item) stackitem.Item {
	a, okA := args[0].(*stackitem.Interop).Value().(blsPoint)
	b, okB := args[1].(*stackitem.Interop).Value().(blsPoint)
	if !(okA && okB) {
		panic("some of the arguments are not a bls12381 point")
	}
	res, err := a.EqualsCheckType(b)
	if err != nil {
		panic(err)
	}
	return stackitem.NewBool(res)
}

func (c *Crypto) bls12381Add(_ *interop.Context, args []stackitem.Item) stackitem.Item {
	a, okA := args[0].(*stackitem.Interop).Value().(blsPoint)
	b, okB := args[1].(*stackitem.Interop).Value().(blsPoint)
	if !(okA && okB) {
		panic("some of the arguments are not a bls12381 point")
	}
	var res interface{}
	switch x := a.point.(type) {
	case *bls12381.G1Affine:
		switch y := b.point.(type) {
		case *bls12381.G1Affine:
			xJac := new(bls12381.G1Jac)
			xJac.FromAffine(x)
			xJac.AddMixed(y)
			res = xJac
		case *bls12381.G1Jac:
			yJac := new(bls12381.G1Jac)
			yJac.Set(y)
			yJac.AddMixed(x)
			res = yJac
		default:
			panic("inconsistent point types")
		}
	case *bls12381.G1Jac:
		resJac := new(bls12381.G1Jac)
		resJac.Set(x)
		switch y := b.point.(type) {
		case *bls12381.G1Affine:
			resJac.AddMixed(y)
		case *bls12381.G1Jac:
			resJac.AddAssign(y)
		default:
			panic("inconsistent")
		}
		res = resJac
	case *bls12381.G2Affine:
		switch y := b.point.(type) {
		case *bls12381.G2Affine:
			xJac := new(bls12381.G2Jac)
			xJac.FromAffine(x)
			xJac.AddMixed(y)
			res = xJac
		case *bls12381.G2Jac:
			yJac := new(bls12381.G2Jac)
			yJac.Set(y)
			yJac.AddMixed(x)
			res = yJac
		default:
			panic("inconsistent")
		}
	case *bls12381.G2Jac:
		resJac := new(bls12381.G2Jac)
		resJac.Set(x)
		switch y := b.point.(type) {
		case *bls12381.G2Affine:
			resJac.AddMixed(y)
		case *bls12381.G2Jac:
			resJac.AddAssign(y)
		default:
			panic("invalid")
		}
		res = resJac
	case *bls12381.GT:
		resGT := new(bls12381.GT)
		resGT.Set(x)
		switch y := b.point.(type) {
		case *bls12381.GT:
			// It's multiplication, see https://github.com/neo-project/Neo.Cryptography.BLS12_381/issues/4.
			resGT.Mul(x, y)
		default:
			panic("invalid")
		}
		res = resGT
	default:
		panic(fmt.Errorf("unexpected bls12381 point type: %T", x))
	}
	return stackitem.NewInterop(blsPoint{point: res})
}

func scalarFromBytes(bytes []byte, neg bool) (*fr.Element, error) {
	alpha := new(fr.Element)
	if len(bytes) != fr.Bytes {
		return nil, fmt.Errorf("invalid multiplier: 32-bytes scalar is expected, got %d", len(bytes))
	}
	// The input bytes are in the LE form, so we can't use fr.Element.SetBytesCanonical as far
	// as it accepts BE.
	v, err := fr.LittleEndian.Element((*[fr.Bytes]byte)(bytes))
	if err != nil {
		return nil, fmt.Errorf("invalid multiplier: failed to decode scalar: %w", err)
	}
	*alpha = v
	if neg {
		alpha.Neg(alpha)
	}
	return alpha, nil
}

func (c *Crypto) bls12381Mul(_ *interop.Context, args []stackitem.Item) stackitem.Item {
	a, okA := args[0].(*stackitem.Interop).Value().(blsPoint)
	if !okA {
		panic("multiplier is not a bls12381 point")
	}
	mulBytes, err := args[1].TryBytes()
	if err != nil {
		panic(fmt.Errorf("invalid multiplier: %w", err))
	}
	neg, err := args[2].TryBool()
	if err != nil {
		panic(fmt.Errorf("invalid negative argument: %w", err))
	}
	alpha, err := scalarFromBytes(mulBytes, neg)
	if err != nil {
		panic(err)
	}
	alphaBi := new(big.Int)
	alpha.BigInt(alphaBi)

	var res interface{}
	switch x := a.point.(type) {
	case *bls12381.G1Affine:
		// The result is in Jacobian form in the reference implementation.
		g1Jac := new(bls12381.G1Jac)
		g1Jac.FromAffine(x)
		g1Jac.ScalarMultiplication(g1Jac, alphaBi)
		res = g1Jac
	case *bls12381.G1Jac:
		g1Jac := new(bls12381.G1Jac)
		g1Jac.ScalarMultiplication(x, alphaBi)
		res = g1Jac
	case *bls12381.G2Affine:
		// The result is in Jacobian form in the reference implementation.
		g2Jac := new(bls12381.G2Jac)
		g2Jac.FromAffine(x)
		g2Jac.ScalarMultiplication(g2Jac, alphaBi)
		res = g2Jac
	case *bls12381.G2Jac:
		g2Jac := new(bls12381.G2Jac)
		g2Jac.ScalarMultiplication(x, alphaBi)
		res = g2Jac
	case *bls12381.GT:
		gt := new(bls12381.GT)

		// C# implementation differs a bit from go's. They use double-and-add algorithm, see
		// https://github.com/neo-project/Neo.Cryptography.BLS12_381/blob/844bc3a4f7d8ba2c545ace90ca124f8ada4c8d29/src/Neo.Cryptography.BLS12_381/Gt.cs#L102
		// and https://en.wikipedia.org/wiki/Elliptic_curve_point_multiplication#Double-and-add,
		// Pay attention that C#'s Gt.Double() squares (not doubles!) the initial GT point.
		// Thus.C#'s scalar multiplication operation over Gt and Scalar is effectively an exponent.
		// Go's exponent algorithm differs a bit from the C#'s double-and-add in that go's one
		// uses 2-bits windowed method for multiplication. However, the resulting GT point is
		// absolutely the same between two implementations.
		gt.Exp(*x, alphaBi)

		res = gt
	default:
		panic(fmt.Errorf("unexpected bls12381 point type: %T", x))
	}
	return stackitem.NewInterop(blsPoint{point: res})
}

func (c *Crypto) bls12381Pairing(_ *interop.Context, args []stackitem.Item) stackitem.Item {
	a, okA := args[0].(*stackitem.Interop).Value().(blsPoint)
	b, okB := args[1].(*stackitem.Interop).Value().(blsPoint)
	if !(okA && okB) {
		panic("some of the arguments are not a bls12381 point")
	}
	var (
		x *bls12381.G1Affine
		y *bls12381.G2Affine
	)
	switch p := a.point.(type) {
	case *bls12381.G1Affine:
		x = p
	case *bls12381.G1Jac:
		x = new(bls12381.G1Affine)
		x.FromJacobian(p)
	default:
		panic(fmt.Errorf("unexpected bls12381 point type (g1): %T", x))
	}
	switch p := b.point.(type) {
	case *bls12381.G2Affine:
		y = p
	case *bls12381.G2Jac:
		y = new(bls12381.G2Affine)
		y.FromJacobian(p)
	default:
		panic(fmt.Errorf("unexpected bls12381 point type (g2): %T", x))
	}
	gt, err := bls12381.Pair([]bls12381.G1Affine{*x}, []bls12381.G2Affine{*y})
	if err != nil {
		panic(fmt.Errorf("failed to perform pairing operation"))
	}
	return stackitem.NewInterop(blsPoint{&gt})
}

// Metadata implements the Contract interface.
func (c *Crypto) Metadata() *interop.ContractMD {
	return &c.ContractMD
}

// Initialize implements the Contract interface.
func (c *Crypto) Initialize(ic *interop.Context) error {
	return nil
}

// InitializeCache implements the Contract interface.
func (c *Crypto) InitializeCache(blockHeight uint32, d *dao.Simple) error {
	return nil
}

// OnPersist implements the Contract interface.
func (c *Crypto) OnPersist(ic *interop.Context) error {
	return nil
}

// PostPersist implements the Contract interface.
func (c *Crypto) PostPersist(ic *interop.Context) error {
	return nil
}
