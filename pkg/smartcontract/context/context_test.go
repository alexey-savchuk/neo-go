package context

import (
	"encoding/hex"
	"testing"

	"github.com/nspcc-dev/neo-go/pkg/config/netmode"
	"github.com/nspcc-dev/neo-go/pkg/core/interop"
	"github.com/nspcc-dev/neo-go/pkg/core/interop/crypto"
	"github.com/nspcc-dev/neo-go/pkg/core/transaction"
	"github.com/nspcc-dev/neo-go/pkg/crypto/keys"
	"github.com/nspcc-dev/neo-go/pkg/internal/testserdes"
	"github.com/nspcc-dev/neo-go/pkg/smartcontract"
	"github.com/nspcc-dev/neo-go/pkg/util"
	"github.com/nspcc-dev/neo-go/pkg/vm"
	"github.com/nspcc-dev/neo-go/pkg/vm/opcode"
	"github.com/nspcc-dev/neo-go/pkg/wallet"
	"github.com/stretchr/testify/require"
)

func TestParameterContext_AddSignatureSimpleContract(t *testing.T) {
	tx := getContractTx()
	priv, err := keys.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PublicKey()
	sig := priv.Sign(tx.GetSignedPart())

	t.Run("invalid contract", func(t *testing.T) {
		c := NewParameterContext("Neo.Core.ContractTransaction", tx)
		ctr := &wallet.Contract{
			Script: pub.GetVerificationScript(),
			Parameters: []wallet.ContractParam{
				newParam(smartcontract.SignatureType, "parameter0"),
				newParam(smartcontract.SignatureType, "parameter1"),
			},
		}
		require.Error(t, c.AddSignature(ctr, pub, sig))
		if item := c.Items[ctr.ScriptHash()]; item != nil {
			require.Nil(t, item.Parameters[0].Value)
		}

		ctr.Parameters = ctr.Parameters[:0]
		require.Error(t, c.AddSignature(ctr, pub, sig))
		if item := c.Items[ctr.ScriptHash()]; item != nil {
			require.Nil(t, item.Parameters[0].Value)
		}
	})

	c := NewParameterContext("Neo.Core.ContractTransaction", tx)
	ctr := &wallet.Contract{
		Script:     pub.GetVerificationScript(),
		Parameters: []wallet.ContractParam{newParam(smartcontract.SignatureType, "parameter0")},
	}
	require.NoError(t, c.AddSignature(ctr, pub, sig))
	item := c.Items[ctr.ScriptHash()]
	require.NotNil(t, item)
	require.Equal(t, sig, item.Parameters[0].Value)

	t.Run("GetWitness", func(t *testing.T) {
		w, err := c.GetWitness(ctr)
		require.NoError(t, err)
		v := newTestVM(w, tx)
		require.NoError(t, v.Run())
		require.Equal(t, 1, v.Estack().Len())
		require.Equal(t, true, v.Estack().Pop().Value())
	})
}

func TestParameterContext_AddSignatureMultisig(t *testing.T) {
	tx := getContractTx()
	c := NewParameterContext("Neo.Core.ContractTransaction", tx)
	privs, pubs := getPrivateKeys(t, 4)
	pubsCopy := keys.PublicKeys(pubs).Copy()
	script, err := smartcontract.CreateMultiSigRedeemScript(3, pubsCopy)
	require.NoError(t, err)

	ctr := &wallet.Contract{
		Script: script,
		Parameters: []wallet.ContractParam{
			newParam(smartcontract.SignatureType, "parameter0"),
			newParam(smartcontract.SignatureType, "parameter1"),
			newParam(smartcontract.SignatureType, "parameter2"),
		},
	}
	data := tx.GetSignedPart()
	priv, err := keys.NewPrivateKey()
	require.NoError(t, err)
	sig := priv.Sign(data)
	require.Error(t, c.AddSignature(ctr, priv.PublicKey(), sig))

	indices := []int{2, 3, 0} // random order
	for _, i := range indices {
		sig := privs[i].Sign(data)
		require.NoError(t, c.AddSignature(ctr, pubs[i], sig))
		require.Error(t, c.AddSignature(ctr, pubs[i], sig))

		item := c.Items[ctr.ScriptHash()]
		require.NotNil(t, item)
		require.Equal(t, sig, item.GetSignature(pubs[i]))
	}

	t.Run("GetWitness", func(t *testing.T) {
		w, err := c.GetWitness(ctr)
		require.NoError(t, err)
		v := newTestVM(w, tx)
		require.NoError(t, v.Run())
		require.Equal(t, 1, v.Estack().Len())
		require.Equal(t, true, v.Estack().Pop().Value())
	})
}

func newTestVM(w *transaction.Witness, tx *transaction.Transaction) *vm.VM {
	ic := &interop.Context{Container: tx}
	crypto.Register(ic)
	v := ic.SpawnVM()
	v.LoadScript(w.VerificationScript)
	v.LoadScript(w.InvocationScript)
	return v
}

func TestParameterContext_MarshalJSON(t *testing.T) {
	priv, err := keys.NewPrivateKey()
	require.NoError(t, err)

	tx := getContractTx()
	data := tx.GetSignedPart()
	sign := priv.Sign(data)

	expected := &ParameterContext{
		Type:       "Neo.Core.ContractTransaction",
		Verifiable: tx,
		Items: map[util.Uint160]*Item{
			priv.GetScriptHash(): {
				Script: priv.GetScriptHash(),
				Parameters: []smartcontract.Parameter{{
					Type:  smartcontract.SignatureType,
					Value: sign,
				}},
				Signatures: map[string][]byte{
					hex.EncodeToString(priv.PublicKey().Bytes()): sign,
				},
			},
		},
	}

	testserdes.MarshalUnmarshalJSON(t, expected, new(ParameterContext))
}

func getPrivateKeys(t *testing.T, n int) ([]*keys.PrivateKey, []*keys.PublicKey) {
	privs := make([]*keys.PrivateKey, n)
	pubs := make([]*keys.PublicKey, n)
	for i := range privs {
		var err error
		privs[i], err = keys.NewPrivateKey()
		require.NoError(t, err)
		pubs[i] = privs[i].PublicKey()
	}
	return privs, pubs
}

func newParam(typ smartcontract.ParamType, name string) wallet.ContractParam {
	return wallet.ContractParam{
		Name: name,
		Type: typ,
	}
}

func getContractTx() *transaction.Transaction {
	tx := transaction.New(netmode.UnitTestNet, []byte{byte(opcode.PUSH1)}, 0)
	tx.Attributes = make([]transaction.Attribute, 0)
	tx.Scripts = make([]transaction.Witness, 0)
	tx.Hash()
	return tx
}
