// Copyright 2021 ChainSafe Systems (ON)
// SPDX-License-Identifier: LGPL-3.0-only

package babe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ChainSafe/gossamer/dot/telemetry"
	"github.com/ChainSafe/gossamer/dot/types"
	"github.com/ChainSafe/gossamer/internal/log"
	"github.com/ChainSafe/gossamer/lib/crypto/sr25519"
	"github.com/ChainSafe/gossamer/lib/runtime"

	ethmetrics "github.com/ethereum/go-ethereum/metrics"
)

var logger = log.NewFromGlobal(log.AddContext("pkg", "babe"))

// Service contains the VRF keys for the validator, as well as BABE configuation data
type Service struct {
	ctx       context.Context
	cancel    context.CancelFunc
	authority bool
	dev       bool
	// lead is used when setting up a new network from genesis.
	// the "lead" node is the node that is designated to build block 1, after which the rest of the nodes
	// will sync block 1 and determine the first slot of the network based on it
	lead bool

	// Storage interfaces
	blockState       BlockState
	storageState     StorageState
	transactionState TransactionState
	epochState       EpochState
	epochLength      uint64

	blockImportHandler BlockImportHandler

	// BABE authority keypair
	keypair *sr25519.Keypair // TODO: change to BABE keystore (#1864)

	// Epoch configuration data
	slotDuration time.Duration
	epochData    *epochData
	// for slots where we are a producer, store the vrf output (bytes 0-32) + proof (bytes 32-96)
	slotToProof map[uint64]*VrfOutputAndProof

	// State variables
	sync.RWMutex
	pause chan struct{}
}

// ServiceConfig represents a BABE configuration
type ServiceConfig struct {
	LogLvl             log.Level
	BlockState         BlockState
	StorageState       StorageState
	TransactionState   TransactionState
	EpochState         EpochState
	BlockImportHandler BlockImportHandler
	Keypair            *sr25519.Keypair
	Runtime            runtime.Instance
	AuthData           []types.Authority
	IsDev              bool
	Authority          bool
	Lead               bool
}

// NewService returns a new Babe Service using the provided VRF keys and runtime
func NewService(cfg *ServiceConfig) (*Service, error) {
	if cfg.Keypair == nil && cfg.Authority {
		return nil, errors.New("cannot create BABE service as authority; no keypair provided")
	}

	if cfg.BlockState == nil {
		return nil, errNilBlockState
	}

	if cfg.EpochState == nil {
		return nil, errNilEpochState
	}

	if cfg.BlockImportHandler == nil {
		return nil, errNilBlockImportHandler
	}

	logger.Patch(log.SetLevel(cfg.LogLvl))

	ctx, cancel := context.WithCancel(context.Background())

	babeService := &Service{
		ctx:                ctx,
		cancel:             cancel,
		blockState:         cfg.BlockState,
		storageState:       cfg.StorageState,
		epochState:         cfg.EpochState,
		keypair:            cfg.Keypair,
		transactionState:   cfg.TransactionState,
		slotToProof:        make(map[uint64]*VrfOutputAndProof),
		pause:              make(chan struct{}),
		authority:          cfg.Authority,
		dev:                cfg.IsDev,
		blockImportHandler: cfg.BlockImportHandler,
		lead:               cfg.Lead,
	}

	epoch, err := cfg.EpochState.GetCurrentEpoch()
	if err != nil {
		return nil, err
	}

	if err = babeService.setupParameters(cfg); err != nil {
		return nil, err
	}

	logger.Debugf(
		"created service with epoch %d, block producer=%t, slot duration %s, epoch length (slots) %d, authorities %v, "+
			"authority index %d, threshold %v and randomness %s",
		epoch, cfg.Authority, babeService.slotDuration, babeService.epochLength,
		Authorities(babeService.epochData.authorities), babeService.epochData.authorityIndex,
		babeService.epochData.threshold, babeService.epochData.randomness)

	if cfg.Lead {
		logger.Debug("node designated to build block 1")
	}

	return babeService, nil
}

func (b *Service) setupParameters(cfg *ServiceConfig) error {
	var err error
	b.epochData = &epochData{}

	epochData, err := b.epochState.GetLatestEpochData()
	if err != nil {
		return err
	}

	b.epochData.randomness = epochData.Randomness
	b.epochData.authorities = epochData.Authorities
	b.slotDuration, err = b.epochState.GetSlotDuration()
	if err != nil {
		return err
	}

	b.epochLength, err = b.epochState.GetEpochLength()
	if err != nil {
		return err
	}

	configData, err := b.epochState.GetLatestConfigData()
	if err != nil {
		return err
	}

	b.epochData.threshold, err = CalculateThreshold(configData.C1, configData.C2, len(b.epochData.authorities))
	if err != nil {
		return err
	}

	if !cfg.Authority {
		return nil
	}

	b.epochData.authorityIndex, err = b.getAuthorityIndex(b.epochData.authorities)
	return err
}

// Start starts BABE block authoring
func (b *Service) Start() error {
	if !b.authority {
		return nil
	}

	// if we aren't leading node, wait for first block
	if !b.lead {
		if err := b.waitForFirstBlock(); err != nil {
			return err
		}
	}

	go b.initiate()
	return nil
}

func (b *Service) waitForFirstBlock() error {
	ch := b.blockState.GetImportedBlockNotifierChannel()
	defer b.blockState.FreeImportedBlockNotifierChannel(ch)

	const firstBlockTimeout = time.Minute * 5
	timer := time.NewTimer(firstBlockTimeout)
	cleanup := func() {
		if !timer.Stop() {
			<-timer.C
		}
	}

	// loop until block 1
	for {
		select {
		case block, ok := <-ch:
			if !ok {
				cleanup()
				return errChannelClosed
			}

			if ok && block.Header.Number.Int64() > 0 {
				cleanup()
				return nil
			}
		case <-timer.C:
			return errFirstBlockTimeout
		case <-b.ctx.Done():
			cleanup()
			return b.ctx.Err()
		}
	}
}

// SlotDuration returns the current service slot duration in milliseconds
func (b *Service) SlotDuration() uint64 {
	return uint64(b.slotDuration.Milliseconds())
}

// EpochLength returns the current service epoch duration
func (b *Service) EpochLength() uint64 {
	return b.epochLength
}

// Pause pauses the service ie. halts block production
func (b *Service) Pause() error {
	b.Lock()
	defer b.Unlock()

	if b.IsPaused() {
		return nil
	}

	close(b.pause)
	return nil
}

// Resume resumes the service ie. resumes block production
func (b *Service) Resume() error {
	b.Lock()
	defer b.Unlock()

	if !b.IsPaused() {
		return nil
	}

	b.pause = make(chan struct{})
	go b.initiate()
	logger.Debug("service resumed")
	return nil
}

// IsPaused returns if the service is paused or not (ie. producing blocks)
func (b *Service) IsPaused() bool {
	select {
	case <-b.pause:
		return true
	default:
		return false
	}
}

// Stop stops the service. If stop is called, it cannot be resumed.
func (b *Service) Stop() error {
	if !b.authority {
		return nil
	}

	b.Lock()
	defer b.Unlock()

	if b.ctx.Err() != nil {
		return errors.New("service already stopped")
	}

	ethmetrics.Unregister(buildBlockTimer)
	ethmetrics.Unregister(buildBlockErrors)

	b.cancel()
	return nil
}

// Authorities returns the current BABE authorities
func (b *Service) Authorities() []types.Authority {
	return b.epochData.authorities
}

// IsStopped returns true if the service is stopped (ie not producing blocks)
func (b *Service) IsStopped() bool {
	return b.ctx.Err() != nil
}

func (b *Service) getAuthorityIndex(Authorities []types.Authority) (uint32, error) {
	if !b.authority {
		return 0, ErrNotAuthority
	}

	pub := b.keypair.Public()

	for i, auth := range Authorities {
		if bytes.Equal(pub.Encode(), auth.Key.Encode()) {
			return uint32(i), nil
		}
	}

	return 0, fmt.Errorf("key not in BABE authority data")
}

func (b *Service) getSlotDuration() time.Duration {
	return b.slotDuration
}

func (b *Service) initiate() {
	if b.blockState == nil {
		logger.Errorf("block authoring: %s", errNilBlockState)
		return
	}

	if b.storageState == nil {
		logger.Errorf("block authoring: %s", errNilStorageState)
		return
	}

	err := b.invokeBlockAuthoring()
	if err != nil {
		logger.Criticalf("block authoring error: %s", err)
	}
}

func (b *Service) invokeBlockAuthoring() error {
	epoch, err := b.epochState.GetCurrentEpoch()
	if err != nil {
		logger.Errorf("failed to get current epoch: %s", err)
		return err
	}

	for {
		err := b.initiateEpoch(epoch)
		if err != nil {
			logger.Errorf("failed to initiate epoch %d: %s", epoch, err)
			return err
		}

		logger.Debugf("initiated epoch with threshold %s, randomness 0x%x and authorities %v",
			b.epochData.threshold, b.epochData.randomness[:], b.epochData.authorities)

		epochStartSlot, err := b.waitForEpochStart(epoch)
		if err != nil {
			logger.Errorf("failed to wait for epoch %d start: %s", epoch, err)
			return err
		}

		// calculate current slot
		startSlot := getCurrentSlot(b.slotDuration)
		intoEpoch := startSlot - epochStartSlot

		// if the calculated amount of slots "into the epoch" is greater than the epoch length,
		// we've been offline for more than an epoch, and need to sync. pause BABE for now, syncer will
		// resume it when ready
		if b.epochLength <= intoEpoch && !b.dev {
			logger.Debugf(
				"pausing BABE, need to sync since we have %d slots (start slot %d) into the epoch starting at %d",
				intoEpoch, startSlot, epochStartSlot)
			return b.Pause()
		}

		if b.dev {
			intoEpoch = intoEpoch % b.epochLength
		}

		logger.Infof("current epoch %d has %d slots", epoch, intoEpoch)

		// get start slot for current epoch
		nextEpochStart, err := b.epochState.GetStartSlotForEpoch(epoch + 1)
		if err != nil {
			logger.Errorf("failed to get start slot for next epoch %d: %s", epoch+1, err)
			return err
		}

		nextEpochStartTime := getSlotStartTime(nextEpochStart, b.slotDuration)
		epochTimer := time.NewTimer(time.Until(nextEpochStartTime))
		cleanup := func() {
			if !epochTimer.Stop() {
				<-epochTimer.C
			}
		}

		slotDone := make([]<-chan time.Time, b.epochLength-intoEpoch)
		for i := 0; i < int(b.epochLength-intoEpoch); i++ {
			slotDone[i] = time.After(b.getSlotDuration() * time.Duration(i))
		}

		for i := 0; i < int(b.epochLength-intoEpoch); i++ {
			done := false

			select {
			case <-b.ctx.Done():
				cleanup()
				return nil
			case <-b.pause:
				cleanup()
				return nil
			case <-slotDone[i]:
				slotNum := startSlot + uint64(i)
				err = b.handleSlot(epoch, slotNum)
				if err == ErrNotAuthorized {
					logger.Debugf(
						"not authorized to produce a block in slot %d, at epoch %d with %d slots in this epoch",
						slotNum, epoch, slotNum-epochStartSlot)
					continue
				} else if err != nil {
					logger.Warnf("failed to handle slot %d: %s", slotNum, err)
					continue
				}
			case <-epochTimer.C:
				done = true
			}

			if done {
				break
			}
		}

		// setup next epoch, re-invoke block authoring
		next, err := b.incrementEpoch()
		if err != nil {
			logger.Errorf("failed to increment epoch: %s", err)
			return err
		}

		logger.Infof("epoch %d complete, upcoming epoch: %d", epoch, next)
		epoch = next
	}
}

func (b *Service) waitForEpochStart(epoch uint64) (uint64, error) {
	// get start slot for current epoch
	epochStart, err := b.epochState.GetStartSlotForEpoch(epoch)
	if err != nil {
		logger.Errorf("failed to get start slot for current epoch %d: %s", epoch, err)
		return 0, err
	}

	epochStartTime := getSlotStartTime(epochStart, b.slotDuration)
	logger.Debugf("checking if epoch started with epoch start %s and current time %s", epochStartTime, time.Now())

	// check if it's time to start the epoch yet. if not, wait until it is
	if time.Since(epochStartTime) < 0 {
		logger.Debug("waiting for epoch to start")
		err = func() error {
			timer := time.NewTimer(time.Until(epochStartTime))
			defer timer.Stop()
			select {
			case <-timer.C:
				return nil
			case <-b.ctx.Done():
				return errors.New("context cancelled")
			case <-b.pause:
				return errors.New("service paused")
			}
		}()

		if err != nil {
			return 0, err
		}
	}

	return epochStart, nil
}

func (b *Service) handleSlot(epoch, slotNum uint64) error {
	if _, has := b.slotToProof[slotNum]; !has {
		return ErrNotAuthorized
	}

	parentHeader, err := b.blockState.BestBlockHeader()
	if err != nil {
		logger.Errorf("block authoring: %s", err)
		return err
	}

	if parentHeader == nil {
		logger.Errorf("block authoring: %s", errNilParentHeader)
		return errNilParentHeader
	}

	// there is a chance that the best block header may change in the course of building the block,
	// so let's copy it first.
	parent, err := parentHeader.DeepCopy()
	if err != nil {
		return err
	}

	currentSlot := Slot{
		start:    time.Now(),
		duration: b.slotDuration,
		number:   slotNum,
	}

	b.storageState.Lock()
	defer b.storageState.Unlock()

	// set runtime trie before building block
	// if block building is successful, store the resulting trie in the storage state
	ts, err := b.storageState.TrieState(&parent.StateRoot)
	if err != nil || ts == nil {
		logger.Errorf("failed to get parent trie with parent state root %s: %s", parent.StateRoot, err)
		return err
	}

	hash := parent.Hash()
	rt, err := b.blockState.GetRuntime(&hash)
	if err != nil {
		return err
	}

	rt.SetContextStorage(ts)

	block, err := b.buildBlock(parent, currentSlot, rt)
	if err != nil {
		return err
	}

	logger.Infof(
		"built block %d with hash %s, state root %s, epoch %d and slot %d",
		block.Header.Number, block.Header.Hash(), block.Header.StateRoot, epoch, slotNum)
	logger.Tracef(
		"built block with parent hash %s, header %s and body %s",
		parent.Hash(), block.Header.String(), block.Body)

	err = telemetry.GetInstance().SendMessage(
		telemetry.NewPreparedBlockForProposingTM(
			block.Header.Hash(),
			block.Header.Number.String(),
		),
	)
	if err != nil {
		logger.Debugf("problem sending 'prepared_block_for_proposing' telemetry message: %s", err)
	}

	if err := b.blockImportHandler.HandleBlockProduced(block, ts); err != nil {
		logger.Warnf("failed to import built block: %s", err)
		return err
	}

	return nil
}

func getCurrentSlot(slotDuration time.Duration) uint64 {
	return uint64(time.Now().UnixNano()) / uint64(slotDuration.Nanoseconds())
}

func getSlotStartTime(slot uint64, slotDuration time.Duration) time.Time {
	return time.Unix(0, int64(slot)*slotDuration.Nanoseconds())
}
