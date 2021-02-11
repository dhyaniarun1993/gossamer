// Copyright 2021 ChainSafe Systems (ON) Corp.
// This file is part of gossamer.
//
// The gossamer library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The gossamer library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the gossamer library. If not, see <http://www.gnu.org/licenses/>.
package modules

import (
	"fmt"
	ctypes "github.com/centrifuge/go-substrate-rpc-client/v2/types"
	"net/http"
)

type AccountModule struct {
	txStateAPI TransactionStateAPI
}

func NewAccountModule(txStateAPI TransactionStateAPI) *AccountModule {
	return &AccountModule{
		txStateAPI: txStateAPI,
	}
}

// U64Response holds U64 response
type U64Response uint64

// StringRequest holds string request
type StringRequest struct {
	String string
}

func (am *AccountModule) NextIndex(r *http.Request, req *StringRequest, res *U64Response) error {
	fmt.Printf("IN NEXT INDEX\n")
	pending := am.txStateAPI.Pending()
	for _, v := range pending {
		var ext ctypes.Extrinsic

		err := ctypes.DecodeFromBytes(v.Extrinsic[1:], &ext)
		if err != nil {
			return err
		}
		fmt.Printf("cext %v\n", &ext)
		fmt.Printf("cextSig %v\n", &ext.Signature.Signer.AsAccountID)

	}
	return nil
}