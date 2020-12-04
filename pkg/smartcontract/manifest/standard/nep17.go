package standard

import (
	"github.com/nspcc-dev/neo-go/pkg/smartcontract"
	"github.com/nspcc-dev/neo-go/pkg/smartcontract/manifest"
)

var nep17 = &manifest.Manifest{
	ABI: manifest.ABI{
		Methods: []manifest.Method{
			{
				Name: "balanceOf",
				Parameters: []manifest.Parameter{
					{Type: smartcontract.Hash160Type},
				},
				ReturnType: smartcontract.IntegerType,
			},
			{
				Name:       "decimals",
				ReturnType: smartcontract.IntegerType,
			},
			{
				Name:       "symbol",
				ReturnType: smartcontract.StringType,
			},
			{
				Name:       "totalSupply",
				ReturnType: smartcontract.IntegerType,
			},
			{
				Name: "transfer",
				Parameters: []manifest.Parameter{
					{Type: smartcontract.Hash160Type},
					{Type: smartcontract.Hash160Type},
					{Type: smartcontract.IntegerType},
					{Type: smartcontract.AnyType},
				},
				ReturnType: smartcontract.BoolType,
			},
		},
		Events: []manifest.Event{
			{
				Name: "Transfer",
				Parameters: []manifest.Parameter{
					{Type: smartcontract.Hash160Type},
					{Type: smartcontract.Hash160Type},
					{Type: smartcontract.IntegerType},
				},
			},
		},
	},
}

// IsNEP17 checks if m is NEP-17 compliant.
func IsNEP17(m *manifest.Manifest) error {
	return Comply(m, nep17)
}