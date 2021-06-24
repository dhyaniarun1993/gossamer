// Copyright 2020 ChainSafe Systems (ON) Corp.
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

package core

import (
	"io/ioutil"
	"math/big"
	"testing"

	"github.com/ChainSafe/gossamer/dot/network"
	"github.com/ChainSafe/gossamer/dot/state"
	"github.com/ChainSafe/gossamer/dot/types"
	"github.com/ChainSafe/gossamer/lib/common"
	"github.com/ChainSafe/gossamer/lib/crypto/sr25519"
	"github.com/ChainSafe/gossamer/lib/genesis"
	"github.com/ChainSafe/gossamer/lib/keystore"
	rtstorage "github.com/ChainSafe/gossamer/lib/runtime/storage"
	"github.com/ChainSafe/gossamer/lib/runtime/wasmer"
	"github.com/ChainSafe/gossamer/lib/trie"
	log "github.com/ChainSafe/log15"

	coremocks "github.com/ChainSafe/gossamer/dot/core/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func newTestGenesisWithTrieAndHeader(t *testing.T) (*genesis.Genesis, *trie.Trie, *types.Header) {
	gen, err := genesis.NewGenesisFromJSONRaw("../../chain/gssmr/genesis.json")
	if err != nil {
		gen, err = genesis.NewGenesisFromJSONRaw("../../../chain/gssmr/genesis.json")
		require.NoError(t, err)
	}

	genTrie, err := genesis.NewTrieFromGenesis(gen)
	require.NoError(t, err)

	genesisHeader, err := types.NewHeader(common.NewHash([]byte{0}), genTrie.MustHash(), trie.EmptyHash, big.NewInt(0), types.Digest{})
	require.NoError(t, err)
	return gen, genTrie, genesisHeader
}

// NewTestService creates a new test core service
func NewTestService(t *testing.T, cfg *Config) *Service {
	if cfg == nil {
		cfg = &Config{}
	}

	cfg.DigestHandler = new(coremocks.MockDigestHandler)
	cfg.DigestHandler.(*coremocks.MockDigestHandler).On("HandleDigests", mock.AnythingOfType("*types.Header"))

	if cfg.Keystore == nil {
		cfg.Keystore = keystore.NewGlobalKeystore()
		kp, err := sr25519.GenerateKeypair()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Keystore.Acco.Insert(kp)
	}

	cfg.LogLvl = 3

	var stateSrvc *state.Service
	testDatadirPath, err := ioutil.TempDir("/tmp", "test-datadir-*")
	require.NoError(t, err)

	gen, genTrie, genHeader := newTestGenesisWithTrieAndHeader(t)

	if cfg.BlockState == nil || cfg.StorageState == nil || cfg.TransactionState == nil || cfg.EpochState == nil || cfg.CodeSubstitutedState == nil {
		config := state.Config{
			Path:     testDatadirPath,
			LogLevel: log.LvlInfo,
		}
		stateSrvc = state.NewService(config)
		stateSrvc.UseMemDB()

		err = stateSrvc.Initialise(gen, genHeader, genTrie)
		require.Nil(t, err)

		err = stateSrvc.Start()
		require.Nil(t, err)
	}

	if cfg.BlockState == nil {
		cfg.BlockState = stateSrvc.Block
	}

	if cfg.StorageState == nil {
		cfg.StorageState = stateSrvc.Storage
	}

	if cfg.TransactionState == nil {
		cfg.TransactionState = stateSrvc.Transaction
	}

	if cfg.EpochState == nil {
		cfg.EpochState = stateSrvc.Epoch
	}

	if cfg.CodeSubstitutedState == nil {
		cfg.CodeSubstitutedState = stateSrvc.Base
	}

	if cfg.Runtime == nil {
		rtCfg := &wasmer.Config{}

		rtCfg.Storage, err = rtstorage.NewTrieState(genTrie)
		require.NoError(t, err)

		rtCfg.CodeHash, err = cfg.StorageState.LoadCodeHash(nil)
		require.NoError(t, err)

		rt, err := wasmer.NewRuntimeFromGenesis(gen, rtCfg)
		require.NoError(t, err)

		cfg.BlockState.StoreRuntime(cfg.BlockState.BestBlockHash(), rt)
	} else {
		cfg.BlockState.StoreRuntime(cfg.BlockState.BestBlockHash(), cfg.Runtime)
	}

	if cfg.Network == nil {
		config := &network.Config{
			BasePath:           testDatadirPath,
			Port:               7001,
			NoBootstrap:        true,
			NoMDNS:             true,
			BlockState:         stateSrvc.Block,
			TransactionHandler: network.NewMockTransactionHandler(),
		}
		cfg.Network = createTestNetworkService(t, config)
	}

	if cfg.CodeSubstitutes == nil {
		cfg.CodeSubstitutes = make(map[common.Hash]string)

		genesisData, err := cfg.CodeSubstitutedState.(*state.BaseState).LoadGenesisData() //nolint
		require.NoError(t, err)

		for k, v := range genesisData.CodeSubstitutes {
			cfg.CodeSubstitutes[common.MustHexToHash(k)] = v
		}
	}

	if cfg.CodeSubstitutedState == nil {
		cfg.CodeSubstitutedState = stateSrvc.Base
	}

	s, err := NewService(cfg)
	require.NoError(t, err)

	if net, ok := cfg.Network.(*network.Service); ok {
		net.SetTransactionHandler(s)
		_ = net.Stop()
	}

	return s
}

// helper method to create and start a new network service
func createTestNetworkService(t *testing.T, cfg *network.Config) (srvc *network.Service) {
	if cfg.LogLvl == 0 {
		cfg.LogLvl = 3
	}

	if cfg.Syncer == nil {
		cfg.Syncer = network.NewMockSyncer()
	}

	srvc, err := network.NewService(cfg)
	require.NoError(t, err)

	err = srvc.Start()
	require.NoError(t, err)

	t.Cleanup(func() {
		err := srvc.Stop()
		require.NoError(t, err)
	})
	return srvc
}
