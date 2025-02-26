package ibft

import (
	"context"
	"math/big"
	"slices"

	"github.com/NilFoundation/nil/nil/common"
	"github.com/NilFoundation/nil/nil/common/logging"
	"github.com/NilFoundation/nil/nil/go-ibft/core"
	"github.com/NilFoundation/nil/nil/go-ibft/messages"
	protoIBFT "github.com/NilFoundation/nil/nil/go-ibft/messages/proto"
	"github.com/NilFoundation/nil/nil/internal/config"
	"github.com/NilFoundation/nil/nil/internal/crypto/bls"
	"github.com/NilFoundation/nil/nil/internal/db"
	"github.com/NilFoundation/nil/nil/internal/execution"
	"github.com/NilFoundation/nil/nil/internal/network"
	"github.com/NilFoundation/nil/nil/internal/types"
	"github.com/rs/zerolog"
)

const ibftProto = "/ibft/0.2"

type ConsensusParams struct {
	ShardId    types.ShardId
	Db         db.DB
	Validator  validator
	NetManager *network.Manager
	PrivateKey bls.PrivateKey
}

type validator interface {
	BuildProposal(ctx context.Context) (*execution.ProposalSSZ, error)
	VerifyProposal(ctx context.Context, proposal *execution.ProposalSSZ) (*types.Block, error)
	InsertProposal(ctx context.Context, proposal *execution.ProposalSSZ, params *types.ConsensusParams) error
	GetLastBlock(ctx context.Context) (*types.Block, common.Hash, error)
}

type backendIBFT struct {
	// `ctx` is the context bound to RunSequence
	ctx context.Context
	// `transportCtx`is the context bound to the transport goroutine
	// It should be used in methods that are called from the transport goroutine with `AddMessage`
	transportCtx    context.Context
	consensus       *core.IBFT
	shardId         types.ShardId
	validator       validator
	logger          zerolog.Logger
	nm              *network.Manager
	transport       transport
	signer          *Signer
	validatorsCache *validatorsMap
}

var _ core.Backend = &backendIBFT{}

func (i *backendIBFT) unmarshalProposal(raw []byte) (*execution.ProposalSSZ, error) {
	proposal := &execution.ProposalSSZ{}
	if err := proposal.UnmarshalSSZ(raw); err != nil {
		return nil, err
	}
	return proposal, nil
}

func (i *backendIBFT) BuildProposal(view *protoIBFT.View) []byte {
	proposal, err := i.validator.BuildProposal(i.ctx)
	if err != nil {
		i.logger.Error().Err(err).Msg("failed to build proposal")
		return nil
	}

	data, err := proposal.MarshalSSZ()
	if err != nil {
		i.logger.Error().Err(err).Msg("failed to marshal proposal")
		return nil
	}

	return data
}

func (i *backendIBFT) buildSignature(committedSeals []*messages.CommittedSeal, height uint64, logger zerolog.Logger) (*types.BlsAggregateSignature, error) {
	validators, err := i.validatorsCache.getValidators(i.ctx, height)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to get validators")
		return nil, err
	}

	pubkeys, err := config.CreateValidatorsPublicKeyMap(validators)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to get validators public keys")
		return nil, err
	}

	mask, err := bls.NewMask(pubkeys.Keys())
	if err != nil {
		logger.Error().Err(err).Msg("Failed to create mask")
		return nil, err
	}

	sigs := make([]bls.Signature, pubkeys.Len())
	for _, seal := range committedSeals {
		index, ok := pubkeys.Find(config.Pubkey(seal.Signer))
		if !ok {
			logger.Error().
				Hex(logging.FieldPublicKey, seal.Signer).
				Msg("Signer not found in validators list")
			return nil, err
		}
		sig, err := bls.SignatureFromBytes(seal.Signature)
		if err != nil {
			logger.Error().Err(err).
				Hex(logging.FieldSignature, seal.Signature).
				Msg("Failed to read signature")
			return nil, err
		}
		if err := mask.SetBit(index, true); err != nil {
			logger.Error().Err(err).Msg("Failed to set index in mask")
			return nil, err
		}
		sigs[index] = sig
	}
	sigs = slices.Collect(common.Filter(slices.Values(sigs), func(sig bls.Signature) bool {
		return sig != nil
	}))

	aggrSig, err := bls.AggregateSignatures(sigs, mask)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to aggregate signatures")
		return nil, err
	}

	aggrSigBytes, err := aggrSig.Marshal()
	if err != nil {
		logger.Error().Err(err).Msg("Failed to marshal aggregated signature")
		return nil, err
	}

	return &types.BlsAggregateSignature{
		Sig:  aggrSigBytes,
		Mask: mask.Bytes(),
	}, nil
}

func (i *backendIBFT) InsertProposal(proposal *protoIBFT.Proposal, committedSeals []*messages.CommittedSeal) {
	proposalBlock, err := i.unmarshalProposal(proposal.RawProposal)
	if err != nil {
		i.logger.Error().Err(err).Msg("failed to unmarshal proposal")
		return
	}

	height := proposalBlock.PrevBlockId.Uint64() + 1

	logger := i.logger.With().
		Uint64(logging.FieldHeight, height).
		Uint64(logging.FieldRound, proposal.Round).
		Logger()

	logger.Trace().Msg("Inserting proposal")

	sig, err := i.buildSignature(committedSeals, height, logger)
	if err != nil {
		return // error is logged in buildSignature
	}

	prevProposerIndex := i.getPrevProposer(height)
	_, proposerIndex, err := i.calcProposer(height, proposal.Round, prevProposerIndex)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to calculate current proposer")
		return
	}

	if err := i.validator.InsertProposal(i.ctx, proposalBlock, &types.ConsensusParams{
		Round:         proposal.Round,
		ProposerIndex: proposerIndex,
		Signature:     sig,
	}); err != nil {
		logger.Error().Err(err).Msg("Failed to insert proposal")
	}
}

func (i *backendIBFT) ID() []byte {
	return i.signer.GetPublicKey()
}

func (i *backendIBFT) isActiveValidator() bool {
	return true
}

func NewConsensus(cfg *ConsensusParams) *backendIBFT {
	logger := logging.NewLogger("consensus").With().Stringer(logging.FieldShardId, cfg.ShardId).Logger()
	l := &ibftLogger{
		logger: logger.With().CallerWithSkipFrameCount(3).Logger(),
	}

	backend := &backendIBFT{
		shardId:         cfg.ShardId,
		validator:       cfg.Validator,
		logger:          logger,
		nm:              cfg.NetManager,
		signer:          NewSigner(cfg.PrivateKey),
		validatorsCache: newValidatorsMap(cfg.Db, cfg.ShardId),
	}
	backend.consensus = core.NewIBFT(l, backend, backend)
	return backend
}

func (i *backendIBFT) Init(ctx context.Context) error {
	i.transportCtx = ctx
	if i.nm == nil {
		i.setupLocalTransport()
		return nil
	}
	return i.setupTransport(ctx)
}

func (i *backendIBFT) GetVotingPowers(height uint64) (map[string]*big.Int, error) {
	validators, err := i.validatorsCache.getValidators(i.ctx, height)
	if err != nil {
		i.logger.Error().
			Err(err).
			Uint64(logging.FieldHeight, height).
			Msg("Failed to get validators")
		return nil, err
	}

	result := make(map[string]*big.Int, len(validators))
	for _, v := range validators {
		result[string(v.PublicKey[:])] = big.NewInt(1)
	}
	return result, nil
}

func (i *backendIBFT) RunSequence(ctx context.Context, height uint64) error {
	i.ctx = ctx
	i.consensus.RunSequence(ctx, height)
	return nil
}
