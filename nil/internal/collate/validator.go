package collate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/NilFoundation/nil/nil/common"
	"github.com/NilFoundation/nil/nil/common/check"
	"github.com/NilFoundation/nil/nil/common/logging"
	"github.com/NilFoundation/nil/nil/internal/config"
	"github.com/NilFoundation/nil/nil/internal/db"
	"github.com/NilFoundation/nil/nil/internal/execution"
	"github.com/NilFoundation/nil/nil/internal/network"
	"github.com/NilFoundation/nil/nil/internal/signer"
	"github.com/NilFoundation/nil/nil/internal/types"
	"github.com/rs/zerolog"
)

var (
	errOldBlock         = errors.New("received old block")
	errOutOfOrder       = errors.New("received block is out of order")
	errHashMismatch     = errors.New("block hash mismatch")
	errInvalidSignature = errors.New("invalid block signature")
)

type Validator struct {
	params *Params

	txFabric       db.DB
	pool           TxnPool
	networkManager *network.Manager
	blockVerifier  *signer.BlockVerifier

	mutex         sync.RWMutex
	lastBlock     *types.Block
	lastBlockHash common.Hash

	logger zerolog.Logger
}

func NewValidator(params *Params, txFabric db.DB, pool TxnPool, nm *network.Manager) *Validator {
	return &Validator{
		params:         params,
		txFabric:       txFabric,
		pool:           pool,
		networkManager: nm,
		blockVerifier:  signer.NewBlockVerifier(params.ShardId, txFabric),
		logger: logging.NewLogger("validator").With().
			Stringer(logging.FieldShardId, params.ShardId).
			Logger(),
	}
}

func (s *Validator) getLastBlockUnlocked(ctx context.Context) (*types.Block, common.Hash, error) {
	if s.lastBlock != nil {
		return s.lastBlock, s.lastBlockHash, nil
	}

	rotx, err := s.txFabric.CreateRoTx(ctx)
	if err != nil {
		return nil, common.EmptyHash, err
	}
	defer rotx.Rollback()

	block, hash, err := db.ReadLastBlock(rotx, s.params.ShardId)
	if err != nil && !errors.Is(err, db.ErrKeyNotFound) {
		return nil, common.EmptyHash, err
	}
	if err == nil {
		return block, hash, err
	}
	return nil, common.EmptyHash, nil
}

func (s *Validator) GetLastBlock(ctx context.Context) (*types.Block, common.Hash, error) {
	s.mutex.RLock()
	lastBlock, lastBlockHash := s.lastBlock, s.lastBlockHash
	s.mutex.RUnlock()

	if lastBlock != nil {
		return lastBlock, lastBlockHash, nil
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.getLastBlockUnlocked(ctx)
}

func (s *Validator) getBlock(ctx context.Context, hash common.Hash) (*types.Block, error) {
	tx, err := s.txFabric.CreateRoTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	block, err := db.ReadBlock(tx, s.params.ShardId, hash)
	if err != nil {
		return nil, err
	}
	return block, nil
}

func (s *Validator) TxPool() TxnPool {
	return s.pool
}

func (s *Validator) BuildProposal(ctx context.Context) (*execution.ProposalSSZ, error) {
	// No lock since it doesn't directly access last block/hash
	proposer := newProposer(s.params, s.params.Topology, s.pool, s.logger)
	proposal, err := proposer.GenerateProposal(ctx, s.txFabric)
	if err != nil {
		return nil, fmt.Errorf("failed to generate proposal: %w", err)
	}
	return proposal, nil
}

func (s *Validator) VerifyProposal(ctx context.Context, proposal *execution.ProposalSSZ) (*types.Block, error) {
	p, err := execution.ConvertProposal(proposal)
	if err != nil {
		return nil, err
	}

	// No lock since it accesses last block/hash only inside "locked" GetLastBlock function
	prevBlock, prevBlockHash, err := s.GetLastBlock(ctx)
	if err != nil {
		return nil, err
	}

	if prevBlockHash != proposal.PrevBlockHash {
		return nil, fmt.Errorf("%w: expected %x, got %x", errHashMismatch, prevBlockHash, proposal.PrevBlockHash)
	}

	gen, err := execution.NewBlockGenerator(ctx, s.params.BlockGeneratorParams, s.txFabric, prevBlock)
	if err != nil {
		return nil, fmt.Errorf("failed to create block generator: %w", err)
	}
	defer gen.Rollback()

	gasPrices := gen.CollectGasPrices(proposal.PrevBlockId)
	res, err := gen.BuildBlock(p, gasPrices)
	if err != nil {
		return nil, fmt.Errorf("failed to generate block: %w", err)
	}
	return res.Block, nil
}

func (s *Validator) InsertProposal(ctx context.Context, proposal *execution.ProposalSSZ, params *types.ConsensusParams) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.insertProposalUnlocked(ctx, proposal, params)
}

func (s *Validator) insertProposalUnlocked(ctx context.Context, proposal *execution.ProposalSSZ, params *types.ConsensusParams) error {
	p, err := execution.ConvertProposal(proposal)
	if err != nil {
		return err
	}
	if err := s.validateProposalUnlocked(ctx, p); err != nil {
		return err
	}

	prevBlock, err := s.getBlock(ctx, proposal.PrevBlockHash)
	if err != nil {
		return err
	}

	gen, err := execution.NewBlockGenerator(ctx, s.params.BlockGeneratorParams, s.txFabric, prevBlock)
	if err != nil {
		return fmt.Errorf("failed to create block generator: %w", err)
	}
	defer gen.Rollback()

	res, err := gen.GenerateBlock(p, params)
	if err != nil {
		return fmt.Errorf("failed to generate block: %w", err)
	}

	s.onBlockCommitUnlocked(ctx, res, p)

	return PublishBlock(ctx, s.networkManager, s.params.ShardId, &types.BlockWithExtractedData{
		Block:           res.Block,
		InTransactions:  res.InTxns,
		OutTransactions: res.OutTxns,
		ChildBlocks:     proposal.ShardHashes,
		Config:          res.ConfigParams,
	})
}

func (s *Validator) onBlockCommitUnlocked(
	ctx context.Context, res *execution.BlockGenerationResult, proposal *execution.Proposal,
) {
	s.setLastBlockUnlocked(res.Block, res.BlockHash)

	if !reflect.ValueOf(s.pool).IsNil() {
		if err := s.pool.OnCommitted(ctx, res.Block.BaseFee, proposal.ExternalTxns); err != nil {
			s.logger.Warn().Err(err).
				Msgf("Failed to remove %d committed transactions from pool", len(proposal.ExternalTxns))
		}
	}
}

func (s *Validator) logBlockDiffError(expected, got *types.Block, expHash, gotHash common.Hash) error {
	msg := fmt.Sprintf("block hash mismatch: expected %x, got %x", expHash, gotHash)
	blockMarshal, err := json.Marshal(got)
	check.PanicIfErr(err)
	lastBlockMarshal, err := json.Marshal(expected)
	check.PanicIfErr(err)
	s.logger.Error().
		Stringer(logging.FieldBlockNumber, expected.Id).
		Stringer("expectedHash", expHash).
		Stringer("gotHash", gotHash).
		RawJSON("expected", blockMarshal).
		RawJSON("got", lastBlockMarshal).
		Msg(msg)
	return returnErrorOrPanic(errors.New(msg))
}

func (s *Validator) validateRepliedBlock(
	in *types.Block, replied *execution.BlockGenerationResult, inHash common.Hash, inTxns []*types.Transaction,
) error {
	if replied.Block.OutTransactionsRoot != in.OutTransactionsRoot {
		return returnErrorOrPanic(fmt.Errorf("out transactions root mismatch. Expected %x, got %x",
			in.OutTransactionsRoot, replied.Block.OutTransactionsRoot))
	}
	if len(replied.OutTxns) != len(inTxns) {
		return returnErrorOrPanic(fmt.Errorf("out transactions count mismatch. Expected %d, got %d",
			len(inTxns), len(replied.InTxns)))
	}
	if replied.Block.ConfigRoot != in.ConfigRoot {
		return returnErrorOrPanic(fmt.Errorf("config root mismatch. Expected %x, got %x",
			in.ConfigRoot, replied.Block.ConfigRoot))
	}
	if replied.BlockHash != inHash {
		return s.logBlockDiffError(in, replied.Block, inHash, replied.BlockHash)
	}
	return nil
}

func (s *Validator) validateProposalUnlocked(ctx context.Context, proposal *execution.Proposal) error {
	lastBlock, lastBlockHash, err := s.getLastBlockUnlocked(ctx)
	if err != nil {
		return err
	}

	blockId := proposal.PrevBlockId + 1
	if blockId <= lastBlock.Id {
		s.logger.Trace().
			Err(errOldBlock).
			Stringer(logging.FieldBlockNumber, blockId).
			Send()
		return errOldBlock
	}

	if blockId != lastBlock.Id+1 {
		s.logger.Debug().
			Stringer(logging.FieldBlockNumber, blockId).
			Msgf("Received block %d is out of order with the last block %d", blockId, lastBlock.Id)
		return errOutOfOrder
	}

	if lastBlockHash != proposal.PrevBlockHash {
		lastBlockMarshal, err := json.Marshal(lastBlock)
		check.PanicIfErr(err)

		s.logger.Error().
			RawJSON("lastBlock", lastBlockMarshal).
			Stringer("lastHash", lastBlockHash).
			Stringer("expectedLastHash", proposal.PrevBlockHash).
			Msgf("Previous block hash mismatch: expected %x, got %x", lastBlockHash, proposal.PrevBlockHash)
		return errHashMismatch
	}

	return nil
}

func (s *Validator) ReplayBlock(ctx context.Context, block *types.BlockWithExtractedData) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.replayBlockUnlocked(ctx, block)
}

func (s *Validator) replayBlockUnlocked(ctx context.Context, block *types.BlockWithExtractedData) error {
	blockHash := block.Block.Hash(s.params.ShardId)
	s.logger.Trace().
		Stringer(logging.FieldBlockNumber, block.Block.Id).
		Stringer(logging.FieldBlockHash, blockHash).
		Msg("Replaying block")

	proposal := &execution.Proposal{
		PrevBlockId:   block.Block.Id - 1,
		PrevBlockHash: block.Block.PrevBlock,
		MainChainHash: block.Block.MainChainHash,
		ShardHashes:   block.ChildBlocks,
	}
	proposal.InternalTxns, proposal.ExternalTxns = execution.SplitInTransactions(block.InTransactions)
	proposal.ForwardTxns, _ = execution.SplitOutTransactions(block.OutTransactions, s.params.ShardId)

	var gasPrices []types.Uint256
	if s.params.ShardId.IsMainShard() {
		if gasPricesBytes, ok := block.Config[config.NameGasPrice]; ok {
			param := &config.ParamGasPrice{}
			if err := param.UnmarshalSSZ(gasPricesBytes); err != nil {
				return fmt.Errorf("failed to unmarshal gas prices: %w", err)
			}
			gasPrices = param.Shards
		}
	}

	if !s.params.DisableConsensus {
		if err := s.blockVerifier.VerifyBlock(ctx, block.Block); err != nil {
			s.logger.Error().
				Uint64(logging.FieldBlockNumber, uint64(block.Id)).
				Stringer(logging.FieldBlockHash, block.Hash(s.params.ShardId)).
				Stringer(logging.FieldShardId, s.params.ShardId).
				Stringer(logging.FieldSignature, block.Signature).
				Err(err).
				Msg("Failed to verify block signature")
			return fmt.Errorf("%w: %w", errInvalidSignature, err)
		}
	}

	if err := s.validateProposalUnlocked(ctx, proposal); err != nil {
		if errors.Is(err, errHashMismatch) {
			return returnErrorOrPanic(err)
		}
		return err
	}

	prevBlock, _, err := s.getLastBlockUnlocked(ctx)
	if err != nil {
		return err
	}

	gen, err := execution.NewBlockGenerator(ctx, s.params.BlockGeneratorParams, s.txFabric, prevBlock)
	if err != nil {
		return err
	}
	defer gen.Rollback()

	// First we build block without writing it into the database, because we need to check that the resulted block is
	// the same as the proposed one.
	resBlock, err := gen.BuildBlock(proposal, gasPrices)
	if err != nil {
		return fmt.Errorf("failed to build block: %w", err)
	}

	// Check generated block and proposed are equal
	if err = s.validateRepliedBlock(block.Block, resBlock, blockHash, block.OutTransactions); err != nil {
		return fmt.Errorf("failed to validate replied block: %w", err)
	}

	// Finally, write generated block into the database
	if err = gen.Finalize(resBlock, &block.ConsensusParams); err != nil {
		return fmt.Errorf("failed to finalize block: %w", err)
	}

	s.onBlockCommitUnlocked(ctx, resBlock, proposal)

	return nil
}

func (s *Validator) setLastBlockUnlocked(block *types.Block, hash common.Hash) {
	s.lastBlock = block
	s.lastBlockHash = hash
}
