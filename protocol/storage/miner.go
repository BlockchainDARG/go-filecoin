package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
	"time"

	inet "gx/ipfs/QmNgLg1NTw37iWbYPKcyK85YJ9Whs1MkPtJwhfqbNYAyKg/go-libp2p-net"
	"gx/ipfs/QmQXze9tG878pa4Euya4rrDpyTNX3kQe4dhCaBzBozGgpe/go-unixfs"
	"gx/ipfs/QmR8BauakNcBa3RbE4nbQu76PDiJgoQgz8AJdhJuiU4TAw/go-cid"
	cbor "gx/ipfs/QmRoARq3nkUb13HSKZGepCZSWe5GrVPwx7xURJGZ7KWv9V/go-ipld-cbor"
	dag "gx/ipfs/QmTQdH4848iTVCJmKXYyRiK72HufWTLYQQ8iN3JaQ8K1Hq/go-merkledag"
	"gx/ipfs/QmVmDhyTTUcQXFD1rRQ64fGLMSAoaQvNH3hwuaCFAPq2hy/errors"
	bserv "gx/ipfs/QmYPZzd9VqmJDwxUnThfeSbV1Y5o53aVPDijTB7j7rS9Ep/go-blockservice"
	"gx/ipfs/QmZNkThpqfVXs9GNbexPrfBbXSLNYeKrE7jwFM2oqHbyqN/go-libp2p-protocol"
	"gx/ipfs/QmaoXrM4Z41PD48JY36YqQGKQpLGjyLA2cKcLsES7YddAq/go-libp2p-host"
	ipld "gx/ipfs/QmcKKBwfz6FyQdHR2jsXrrF6XeSBXYL86anmWNewpFpoF5/go-ipld-format"
	"gx/ipfs/QmcTzQXRcU2vf8yX5EEboz1BSvWC7wWmeYAKVQmhp8WZYU/sha256-simd"
	logging "gx/ipfs/QmcuXC5cxs79ro2cUuHs4HQ2bkDLJUYokwL8aivcX6HW3C/go-log"
	"gx/ipfs/Qmf4xQhNomPNhrtZc67qSnfJSjxjXs9LWvknJtSXwimPrM/go-datastore"
	"gx/ipfs/Qmf4xQhNomPNhrtZc67qSnfJSjxjXs9LWvknJtSXwimPrM/go-datastore/query"

	"github.com/filecoin-project/go-filecoin/abi"
	"github.com/filecoin-project/go-filecoin/actor/builtin/miner"
	"github.com/filecoin-project/go-filecoin/actor/builtin/paymentbroker"
	"github.com/filecoin-project/go-filecoin/address"
	cbu "github.com/filecoin-project/go-filecoin/cborutil"
	"github.com/filecoin-project/go-filecoin/exec"
	"github.com/filecoin-project/go-filecoin/proofs"
	"github.com/filecoin-project/go-filecoin/proofs/sectorbuilder"
	"github.com/filecoin-project/go-filecoin/repo"
	"github.com/filecoin-project/go-filecoin/types"
	"github.com/filecoin-project/go-filecoin/util/convert"
)

var log = logging.Logger("/fil/storage")

const makeDealProtocol = protocol.ID("/fil/storage/mk/1.0.0")
const queryDealProtocol = protocol.ID("/fil/storage/qry/1.0.0")

// TODO: replace this with a queries to pick reasonable gas price and limits.
const submitPostGasPrice = 0
const submitPostGasLimit = 300

const waitForPaymentChannelDuration = 2 * time.Minute

const minerDatastorePrefix = "miner"
const dealsAwatingSealDatastorePrefix = "dealsAwaitingSeal"

// Miner represents a storage miner.
type Miner struct {
	minerAddr      address.Address
	minerOwnerAddr address.Address

	// deals is a list of deals we made. It is indexed by the CID of the proposal.
	deals   map[cid.Cid]*storageDeal
	dealsDs repo.Datastore
	dealsLk sync.Mutex

	postInProcessLk sync.Mutex
	postInProcess   *types.BlockHeight

	dealsAwaitingSeal *dealsAwaitingSealStruct

	porcelainAPI minerPorcelain
	node         node

	proposalAcceptor func(ctx context.Context, m *Miner, p *DealProposal) (*DealResponse, error)
	proposalRejector func(ctx context.Context, m *Miner, p *DealProposal, reason string) (*DealResponse, error)
}

type storageDeal struct {
	Proposal *DealProposal
	Response *DealResponse
}

// minerPorcelain is the subset of the porcelain API that storage.Miner needs.
type minerPorcelain interface {
	ChainBlockHeight(ctx context.Context) (*types.BlockHeight, error)
	ConfigGet(dottedPath string) (interface{}, error)

	MessageSend(ctx context.Context, from, to address.Address, value *types.AttoFIL, gasPrice types.AttoFIL, gasLimit types.GasUnits, method string, params ...interface{}) (cid.Cid, error)
	MessageQuery(ctx context.Context, optFrom, to address.Address, method string, params ...interface{}) ([][]byte, *exec.FunctionSignature, error)
	MessageWait(ctx context.Context, msgCid cid.Cid, cb func(*types.Block, *types.SignedMessage, *types.MessageReceipt) error) error
}

// node is subset of node on which this protocol depends. These deps
// are moving off of node and into the porcelain api (see porcelainAPI). Eventually this
// dependency on node should go away, fully replaced by the dependency on the porcelain api.
type node interface {
	BlockHeight() (*types.BlockHeight, error)
	GetBlockTime() time.Duration
	BlockService() bserv.BlockService
	Host() host.Host
	SectorBuilder() sectorbuilder.SectorBuilder
}

// generatePostInput is a struct containing sector id and related commitments
// used to generate a proof-of-spacetime
type generatePostInput struct {
	commD     proofs.CommD
	commR     proofs.CommR
	commRStar proofs.CommRStar
	sectorID  uint64
}

func init() {
	cbor.RegisterCborType(storageDeal{})
	cbor.RegisterCborType(dealsAwaitingSealStruct{})
}

// NewMiner is
func NewMiner(ctx context.Context, minerAddr, minerOwnerAddr address.Address, nd node, dealsDs repo.Datastore, porcelainAPI minerPorcelain) (*Miner, error) {
	sm := &Miner{
		minerAddr:        minerAddr,
		minerOwnerAddr:   minerOwnerAddr,
		deals:            make(map[cid.Cid]*storageDeal),
		porcelainAPI:     porcelainAPI,
		dealsDs:          dealsDs,
		node:             nd,
		proposalAcceptor: acceptProposal,
		proposalRejector: rejectProposal,
	}

	if err := sm.loadDealsAwaitingSeal(); err != nil {
		return nil, errors.Wrap(err, "failed to load dealAwaitingSeal when creating miner")
	}
	sm.dealsAwaitingSeal.onSuccess = sm.onCommitSuccess
	sm.dealsAwaitingSeal.onFail = sm.onCommitFail

	if err := sm.loadDeals(); err != nil {
		return nil, errors.Wrap(err, "failed to load miner deals when creating miner")
	}

	nd.Host().SetStreamHandler(makeDealProtocol, sm.handleMakeDeal)
	nd.Host().SetStreamHandler(queryDealProtocol, sm.handleQueryDeal)

	return sm, nil
}

func (sm *Miner) handleMakeDeal(s inet.Stream) {
	defer s.Close() // nolint: errcheck

	var proposal DealProposal
	if err := cbu.NewMsgReader(s).ReadMsg(&proposal); err != nil {
		log.Errorf("received invalid proposal: %s", err)
		return
	}

	ctx := context.Background()
	resp, err := sm.receiveStorageProposal(ctx, &proposal)
	if err != nil {
		log.Errorf("failed to process proposal: %s", err)
		return
	}

	if err := cbu.NewMsgWriter(s).WriteMsg(resp); err != nil {
		log.Errorf("failed to write proposal response: %s", err)
	}
}

// receiveStorageProposal is the entry point for the miner storage protocol
func (sm *Miner) receiveStorageProposal(ctx context.Context, p *DealProposal) (*DealResponse, error) {
	// TODO: Check signature

	if err := sm.validateDealPayment(ctx, p); err != nil {
		return sm.proposalRejector(ctx, sm, p, err.Error())
	}

	// Payment is valid, everything else checks out, let's accept this proposal
	return sm.proposalAcceptor(ctx, sm, p)
}

func (sm *Miner) validateDealPayment(ctx context.Context, p *DealProposal) error {
	// compute expected total price for deal (storage price * duration * bytes)
	price, err := sm.getStoragePrice()
	if err != nil {
		return err
	}

	if p.Size == nil {
		return fmt.Errorf("proposed deal has no size")
	}

	durationBigInt := big.NewInt(0).SetUint64(p.Duration)
	priceBigInt := big.NewInt(0).SetUint64(p.Size.Uint64())
	expectedPrice := price.MulBigInt(durationBigInt).MulBigInt(priceBigInt)
	if p.TotalPrice.LessThan(expectedPrice) {
		return fmt.Errorf("proposed price (%s) is less than expected (%s) given asking price of %s", p.TotalPrice.String(), expectedPrice.String(), price.String())
	}

	// get channel
	channel, err := sm.getPaymentChannel(ctx, p)
	if err != nil {
		return err
	}

	// confirm we are target of channel
	if channel.Target != sm.minerOwnerAddr {
		return fmt.Errorf("miner account (%s) is not target of payment channel (%s)", sm.minerOwnerAddr.String(), channel.Target.String())
	}

	// confirm channel contains enough funds
	if channel.Amount.LessThan(expectedPrice) {
		return fmt.Errorf("payment channel does not contain enough funds (%s < %s)", channel.Amount.String(), expectedPrice.String())
	}

	// start with current block height
	blockHeight, err := sm.porcelainAPI.ChainBlockHeight(ctx)
	if err != nil {
		return fmt.Errorf("could not get current block height")
	}

	// require at least one payment
	if len(p.Payment.Vouchers) < 1 {
		return errors.New("deal proposal contains no payment vouchers")
	}

	// first payment must be before blockHeight + VoucherInterval
	expectedFirstPayment := blockHeight.Add(types.NewBlockHeight(VoucherInterval))
	firstPayment := p.Payment.Vouchers[0].ValidAt
	if firstPayment.GreaterThan(expectedFirstPayment) {
		return errors.New("payments start after deal start interval")
	}

	lastValidAt := expectedFirstPayment
	for _, v := range p.Payment.Vouchers {
		// confirm signature is valid against expected actor and channel id
		if !paymentbroker.VerifyVoucherSignature(p.Payment.Payer, p.Payment.Channel, &v.Amount, &v.ValidAt, v.Signature) {
			return errors.New("invalid signature in voucher")
		}

		// make sure voucher validAt is not spaced to far apart
		expectedValidAt := lastValidAt.Add(types.NewBlockHeight(VoucherInterval))
		if v.ValidAt.GreaterThan(expectedValidAt) {
			return fmt.Errorf("interval between vouchers too high (%s - %s > %d)", v.ValidAt.String(), lastValidAt.String(), VoucherInterval)
		}

		// confirm voucher amounts increase linearly
		// We want the ratio of voucher amount / (valid at - expected start) >= total price / duration
		// this is implied by amount*duration >= total price*(valid at - expected start).
		lhs := v.Amount.MulBigInt(big.NewInt(int64(p.Duration)))
		rhs := p.TotalPrice.MulBigInt(v.ValidAt.Sub(blockHeight).AsBigInt())
		if lhs.LessThan(rhs) {
			return fmt.Errorf("voucher amount (%s) less than expected for voucher valid at (%s)", v.Amount.String(), v.ValidAt.String())
		}

		lastValidAt = &v.ValidAt
	}

	// confirm last voucher value is for full amount
	lastVoucher := p.Payment.Vouchers[len(p.Payment.Vouchers)-1]
	if lastVoucher.Amount.LessThan(p.TotalPrice) {
		return fmt.Errorf("last payment (%s) does not cover total price (%s)", lastVoucher.Amount.String(), p.TotalPrice.String())
	}

	// require channel expires at or after last voucher + ChannelExpiryInterval
	expectedEol := lastVoucher.ValidAt.Add(types.NewBlockHeight(ChannelExpiryInterval))
	if channel.Eol.LessThan(expectedEol) {
		return fmt.Errorf("payment channel eol (%s) less than required eol (%s)", channel.Eol, expectedEol)
	}

	return nil
}

func (sm *Miner) getStoragePrice() (*types.AttoFIL, error) {
	storagePrice, err := sm.porcelainAPI.ConfigGet("mining.storagePrice")
	if err != nil {
		return nil, err
	}
	storagePriceAF, ok := storagePrice.(*types.AttoFIL)
	if !ok {
		return nil, errors.New("Could not retrieve storagePrice from config")
	}
	return storagePriceAF, nil
}

// some parts of this should be porcelain
func (sm *Miner) getPaymentChannel(ctx context.Context, p *DealProposal) (*paymentbroker.PaymentChannel, error) {
	// wait for create channel message
	messageCid := p.Payment.ChannelMsgCid

	waitCtx, waitCancel := context.WithDeadline(ctx, time.Now().Add(waitForPaymentChannelDuration))
	err := sm.porcelainAPI.MessageWait(waitCtx, *messageCid, func(blk *types.Block, smsg *types.SignedMessage, receipt *types.MessageReceipt) error {
		return nil
	})
	waitCancel()
	if err != nil {
		if err == context.DeadlineExceeded {
			return nil, errors.Wrap(err, "Timeout waiting for payment channel")
		}
		return nil, err
	}

	payer := p.Payment.Payer

	ret, _, err := sm.porcelainAPI.MessageQuery(ctx, address.Address{}, address.PaymentBrokerAddress, "ls", payer)
	if err != nil {
		return nil, errors.Wrap(err, "Error getting payment channel for payer")
	}

	var channels map[string]*paymentbroker.PaymentChannel
	if err := cbor.DecodeInto(ret[0], &channels); err != nil {
		return nil, errors.Wrap(err, "Could not decode payment channels for payer")
	}
	channel, ok := channels[p.Payment.Channel.KeyString()]
	if !ok {
		return nil, fmt.Errorf("could not find payment channel for payer %s and id %s", payer.String(), p.Payment.Channel.KeyString())
	}
	return channel, nil
}

func acceptProposal(ctx context.Context, sm *Miner, p *DealProposal) (*DealResponse, error) {
	if sm.node.SectorBuilder() == nil {
		return nil, errors.New("Mining disabled, can not process proposal")
	}

	proposalCid, err := convert.ToCid(p)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cid of proposal")
	}

	resp := &DealResponse{
		State:       Accepted,
		ProposalCid: proposalCid,
		Signature:   types.Signature("signaturrreee"),
	}

	sm.dealsLk.Lock()
	defer sm.dealsLk.Unlock()

	sm.deals[proposalCid] = &storageDeal{
		Proposal: p,
		Response: resp,
	}
	if err := sm.saveDeal(proposalCid); err != nil {
		sm.deals[proposalCid].Response.State = Failed
		sm.deals[proposalCid].Response.Message = "Could not persist deal due to internal error"
		return nil, errors.Wrap(err, "failed to save miner deal")
	}

	// TODO: use some sort of nicer scheduler
	go sm.processStorageDeal(proposalCid)

	return resp, nil
}

func rejectProposal(ctx context.Context, sm *Miner, p *DealProposal, reason string) (*DealResponse, error) {
	proposalCid, err := convert.ToCid(p)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cid of proposal")
	}

	resp := &DealResponse{
		State:       Rejected,
		ProposalCid: proposalCid,
		Message:     reason,
		Signature:   types.Signature("signaturrreee"),
	}

	sm.dealsLk.Lock()
	defer sm.dealsLk.Unlock()

	sm.deals[proposalCid] = &storageDeal{
		Proposal: p,
		Response: resp,
	}
	if err := sm.saveDeal(proposalCid); err != nil {
		return nil, errors.Wrap(err, "failed to save miner deal")
	}

	return resp, nil
}

func (sm *Miner) getStorageDeal(c cid.Cid) *storageDeal {
	sm.dealsLk.Lock()
	defer sm.dealsLk.Unlock()
	return sm.deals[c]
}

func (sm *Miner) updateDealResponse(proposalCid cid.Cid, f func(*DealResponse)) error {
	sm.dealsLk.Lock()
	defer sm.dealsLk.Unlock()
	f(sm.deals[proposalCid].Response)
	err := sm.saveDeal(proposalCid)
	if err != nil {
		return errors.Wrap(err, "failed to store updated deal response in datastore")
	}

	log.Debugf("Miner.updateDealResponse(%s) - %d", proposalCid.String(), sm.deals[proposalCid].Response)
	return nil
}

func (sm *Miner) processStorageDeal(c cid.Cid) {
	log.Debugf("Miner.processStorageDeal(%s)", c.String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := sm.getStorageDeal(c)
	if d.Response.State != Accepted {
		// TODO: handle resumption of deal processing across miner restarts
		log.Error("attempted to process an already started deal")
		return
	}

	// 'Receive' the data, this could also be a truck full of hard drives. (TODO: proper abstraction)
	// TODO: this is not a great way to do this. At least use a session
	// Also, this needs to be fetched into a staging area for miners to prepare and seal in data
	log.Debug("Miner.processStorageDeal - FetchGraph")
	if err := dag.FetchGraph(ctx, d.Proposal.PieceRef, dag.NewDAGService(sm.node.BlockService())); err != nil {
		log.Errorf("failed to fetch data: %s", err)
		err := sm.updateDealResponse(c, func(resp *DealResponse) {
			resp.Message = "Transfer failed"
			resp.State = Failed
			// TODO: signature?
		})
		if err != nil {
			log.Errorf("could not update to deal to 'Failed' state: %s", err)
		}
		return
	}

	fail := func(message, logerr string) {
		log.Errorf(logerr)
		err := sm.updateDealResponse(c, func(resp *DealResponse) {
			resp.Message = message
			resp.State = Failed
		})
		if err != nil {
			log.Errorf("could not update to deal to 'Failed' state in fail callback: %s", err)
		}
	}

	pi := &sectorbuilder.PieceInfo{
		Ref:  d.Proposal.PieceRef,
		Size: d.Proposal.Size.Uint64(),
	}

	// There is a race here that requires us to use dealsAwaitingSeal below. If the
	// sector gets sealed and OnCommitmentAddedToChain is called right after
	// AddPiece returns but before we record the sector/deal mapping we might
	// miss it. Hence, dealsAwaitingSealStruct. I'm told that sealing in practice is
	// so slow that the race only exists in tests, but tests were flaky so
	// we fixed it with dealsAwaitingSealStruct.
	//
	// Also, this pattern of not being able to set up book-keeping ahead of
	// the call is inelegant.
	sectorID, err := sm.node.SectorBuilder().AddPiece(ctx, pi)
	if err != nil {
		fail("failed to submit seal proof", fmt.Sprintf("failed to add piece: %s", err))
		return
	}

	err = sm.updateDealResponse(c, func(resp *DealResponse) {
		resp.State = Staged
	})
	if err != nil {
		log.Errorf("could update to 'Staged': %s", err)
	}

	// Careful: this might update state to success or failure so it should go after
	// updating state to Staged.
	sm.dealsAwaitingSeal.add(sectorID, c)
	if err := sm.saveDealsAwaitingSeal(); err != nil {
		log.Errorf("could not save deal awaiting seal: %s", err)
	}
}

// dealsAwaitingSealStruct is a container for keeping track of which sectors have
// pieces from which deals. We need it to accommodate a race condition where
// a sector commit message is added to chain before we can add the sector/deal
// book-keeping. It effectively caches success and failure results for sectors
// for tardy add() calls.
type dealsAwaitingSealStruct struct {
	l sync.Mutex
	// Maps from sector id to the deal cids with pieces in the sector.
	SectorsToDeals map[uint64][]cid.Cid
	// Maps from sector id to sector.
	SuccessfulSectors map[uint64]*sectorbuilder.SealedSectorMetadata
	// Maps from sector id to seal failure error string.
	FailedSectors map[uint64]string

	onSuccess func(dealCid cid.Cid, sector *sectorbuilder.SealedSectorMetadata)
	onFail    func(dealCid cid.Cid, message string)
}

func (sm *Miner) loadDealsAwaitingSeal() error {
	sm.dealsAwaitingSeal = &dealsAwaitingSealStruct{
		SectorsToDeals:    make(map[uint64][]cid.Cid),
		SuccessfulSectors: make(map[uint64]*sectorbuilder.SealedSectorMetadata),
		FailedSectors:     make(map[uint64]string),
	}

	key := datastore.KeyWithNamespaces([]string{dealsAwatingSealDatastorePrefix})
	result, notFound := sm.dealsDs.Get(key)
	if notFound == nil {
		if err := json.Unmarshal(result, &sm.dealsAwaitingSeal); err != nil {
			return errors.Wrap(err, "failed to unmarshal deals awaiting seal from datastore")
		}
	}

	return nil
}

func (sm *Miner) saveDealsAwaitingSeal() error {
	marshalledDealsAwaitingSeal, err := json.Marshal(sm.dealsAwaitingSeal)
	if err != nil {
		return errors.Wrap(err, "Could not marshal dealsAwaitingSeal")
	}
	key := datastore.KeyWithNamespaces([]string{dealsAwatingSealDatastorePrefix})
	err = sm.dealsDs.Put(key, marshalledDealsAwaitingSeal)
	if err != nil {
		return errors.Wrap(err, "could not save deal awaiting seal record to disk, in-memory deals differ from persisted deals!")
	}

	return nil
}

func (dealsAwaitingSeal *dealsAwaitingSealStruct) add(sectorID uint64, dealCid cid.Cid) {
	dealsAwaitingSeal.l.Lock()
	defer dealsAwaitingSeal.l.Unlock()

	if sector, ok := dealsAwaitingSeal.SuccessfulSectors[sectorID]; ok {
		dealsAwaitingSeal.onSuccess(dealCid, sector)
		// Don't keep references to sectors around forever. Assume that at most
		// one success-before-add call will happen (eg, in a test). Sector sealing
		// outside of tests is so slow that it shouldn't happen in practice.
		// So now that it has happened once, clean it up. If we wanted to keep
		// the state around for longer for some reason we need to limit how many
		// sectors we hang onto, eg keep a fixed-length slice of successes
		// and failures and shift the oldest off and the newest on.
		delete(dealsAwaitingSeal.SuccessfulSectors, sectorID)
	} else if message, ok := dealsAwaitingSeal.FailedSectors[sectorID]; ok {
		dealsAwaitingSeal.onFail(dealCid, message)
		// Same as above.
		delete(dealsAwaitingSeal.FailedSectors, sectorID)
	} else {
		deals, ok := dealsAwaitingSeal.SectorsToDeals[sectorID]
		if ok {
			dealsAwaitingSeal.SectorsToDeals[sectorID] = append(deals, dealCid)
		} else {
			dealsAwaitingSeal.SectorsToDeals[sectorID] = []cid.Cid{dealCid}
		}
	}
}

func (dealsAwaitingSeal *dealsAwaitingSealStruct) success(sector *sectorbuilder.SealedSectorMetadata) {
	dealsAwaitingSeal.l.Lock()
	defer dealsAwaitingSeal.l.Unlock()

	dealsAwaitingSeal.SuccessfulSectors[sector.SectorID] = sector

	for _, dealCid := range dealsAwaitingSeal.SectorsToDeals[sector.SectorID] {
		dealsAwaitingSeal.onSuccess(dealCid, sector)
	}
	delete(dealsAwaitingSeal.SectorsToDeals, sector.SectorID)
}

func (dealsAwaitingSeal *dealsAwaitingSealStruct) fail(sectorID uint64, message string) {
	dealsAwaitingSeal.l.Lock()
	defer dealsAwaitingSeal.l.Unlock()

	dealsAwaitingSeal.FailedSectors[sectorID] = message

	for _, dealCid := range dealsAwaitingSeal.SectorsToDeals[sectorID] {
		dealsAwaitingSeal.onFail(dealCid, message)
	}
	delete(dealsAwaitingSeal.SectorsToDeals, sectorID)
}

// OnCommitmentAddedToChain is a callback, called when a sector seal message was posted to the chain.
func (sm *Miner) OnCommitmentAddedToChain(sector *sectorbuilder.SealedSectorMetadata, err error) {
	sectorID := sector.SectorID
	log.Debug("Miner.OnCommitmentAddedToChain")

	if err != nil {
		errMsg := fmt.Sprintf("failed sealing sector: %v: %s:", sectorID, err)
		log.Error(errMsg)
		sm.dealsAwaitingSeal.fail(sector.SectorID, errMsg)
	} else {
		sm.dealsAwaitingSeal.success(sector)
	}
	if err := sm.saveDealsAwaitingSeal(); err != nil {
		errMsg := fmt.Sprintf("failed persisting deals awaiting seal: %s", err)
		log.Error(errMsg)
		sm.dealsAwaitingSeal.fail(sector.SectorID, errMsg)
	}
}

func (sm *Miner) onCommitSuccess(dealCid cid.Cid, sector *sectorbuilder.SealedSectorMetadata) {
	err := sm.updateDealResponse(dealCid, func(resp *DealResponse) {
		resp.State = Posted
		resp.ProofInfo = &ProofInfo{
			SectorID: sector.SectorID,
			CommR:    sector.CommR[:],
			CommD:    sector.CommD[:],
		}
	})
	if err != nil {
		log.Errorf("commit succeeded but could not update to deal 'Posted' state: %s", err)
	}
}

func (sm *Miner) onCommitFail(dealCid cid.Cid, message string) {
	err := sm.updateDealResponse(dealCid, func(resp *DealResponse) {
		resp.Message = message
		resp.State = Failed
	})
	log.Errorf("commit failure but could not update to deal 'Failed' state: %s", err)
}

// OnNewHeaviestTipSet is a callback called by node, every time the the latest
// head is updated. It is used to check if we are in a new proving period and
// need to trigger PoSt submission.
func (sm *Miner) OnNewHeaviestTipSet(ts types.TipSet) {
	ctx := context.Background()

	rets, sig, err := sm.porcelainAPI.MessageQuery(
		ctx,
		address.Address{},
		sm.minerAddr,
		"getSectorCommitments",
	)
	if err != nil {
		log.Errorf("failed to call query method getSectorCommitments: %s", err)
		return
	}

	commitmentsVal, err := abi.Deserialize(rets[0], sig.Return[0])
	if err != nil {
		log.Errorf("failed to convert returned ABI value: %s", err)
		return
	}

	commitments, ok := commitmentsVal.Val.(map[string]types.Commitments)
	if !ok {
		log.Errorf("failed to convert returned ABI value to miner.Commitments")
		return
	}

	var commRs []proofs.CommR
	for _, v := range commitments {
		commRs = append(commRs, v.CommR)
	}

	if len(commRs) == 0 {
		// no sector sealed, nothing to do
		return
	}

	challengeSeed, err := sm.provingPeriodPoStChallengeSeed()
	if err != nil {
		log.Errorf("failed to generate PoSt challenge seed: %s", err)
		return
	}

	provingPeriodStart, err := sm.getProvingPeriodStart()
	if err != nil {
		log.Errorf("failed to get provingPeriodStart: %s", err)
		return
	}

	sm.postInProcessLk.Lock()
	defer sm.postInProcessLk.Unlock()

	if sm.postInProcess != nil && sm.postInProcess.Equal(provingPeriodStart) {
		// post is already being generated for this period, nothing to do
		return
	}

	height, err := ts.Height()
	if err != nil {
		log.Errorf("failed to get block height: %s", err)
		return
	}

	h := types.NewBlockHeight(height)
	provingPeriodEnd := provingPeriodStart.Add(miner.ProvingPeriodBlocks)

	if h.GreaterEqual(provingPeriodStart) {
		if h.LessThan(provingPeriodEnd) {
			// we are in a new proving period, lets get this post going
			sm.postInProcess = provingPeriodStart
			go sm.generateAndSubmitPoSt(provingPeriodStart, provingPeriodEnd, challengeSeed, commRs)
		} else {
			// we are too late
			// TODO: figure out faults and payments here
			log.Errorf("too late start=%s  end=%s current=%s", provingPeriodStart, provingPeriodEnd, h)
		}
	}
}

func (sm *Miner) getProvingPeriodStart() (*types.BlockHeight, error) {
	res, _, err := sm.porcelainAPI.MessageQuery(
		context.Background(),
		address.Address{},
		sm.minerAddr,
		"getProvingPeriodStart",
	)
	if err != nil {
		return nil, err
	}

	return types.NewBlockHeightFromBytes(res[0]), nil
}

// provingPeriodPoStChallengeSeed produces the proof-of-spacetime
// seed for the miner's current proving period.
func (sm *Miner) provingPeriodPoStChallengeSeed() (proofs.PoStChallengeSeed, error) {
	height, err := sm.getProvingPeriodStart()
	if err != nil {
		return proofs.PoStChallengeSeed{}, errors.Wrap(err, "failed to get miner proving period start block height")
	}

	return sha256.Sum256(height.Bytes()), nil
}

// generateAndSubmitPoSt generates a proof-of-spacetime and adds the
// corresponding submitPoSt message to the node's message pool.
func (sm *Miner) generateAndSubmitPoSt(provingPeriodStart, provingPeriodEnd *types.BlockHeight, challengeSeed proofs.PoStChallengeSeed, commRs []proofs.CommR) {
	req := sectorbuilder.GeneratePoSTRequest{
		CommRs:        commRs,
		ChallengeSeed: challengeSeed,
	}
	res, err := sm.node.SectorBuilder().GeneratePoST(req)
	if err != nil {
		log.Errorf("failed to generate PoSts: %s", err)
		return
	}
	if len(res.Faults) != 0 {
		log.Warningf("some faults when generating PoSt: %v", res.Faults)
		// TODO: proper fault handling
	}

	height, err := sm.node.BlockHeight()
	if err != nil {
		log.Errorf("failed to submit PoSt, as the current block height can not be determined: %s", err)
		// TODO: what should happen in this case?
		return
	}

	if height.LessThan(provingPeriodStart) {
		// TODO: what to do here? not sure this can happen, maybe through reordering?
		log.Errorf("PoSt generation time took negative block time: %s < %s", height, provingPeriodStart)
		return
	}

	if height.GreaterEqual(provingPeriodEnd) {
		// TODO: we are too late, figure out faults and decide if we want to still submit
		log.Errorf("PoSt generation was too slow height=%s provingPeriodEnd=%s", height, provingPeriodEnd)
		return
	}

	// TODO: figure out a more sensible timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// TODO: algorithmically determine appropriate values for these
	gasPrice := types.NewGasPrice(submitPostGasPrice)
	gasLimit := types.NewGasUnits(submitPostGasLimit)

	_, err = sm.porcelainAPI.MessageSend(ctx, sm.minerOwnerAddr, sm.minerAddr, types.ZeroAttoFIL, gasPrice, gasLimit, "submitPoSt", res.Proof[:])
	if err != nil {
		log.Errorf("failed to submit PoSt: %s", err)
		return
	}

	log.Debug("submitted PoSt")
}

// Query responds to a query for the proposal referenced by the given cid
func (sm *Miner) Query(ctx context.Context, c cid.Cid) *DealResponse {
	sm.dealsLk.Lock()
	defer sm.dealsLk.Unlock()
	d, ok := sm.deals[c]
	if !ok {
		return &DealResponse{
			State:   Unknown,
			Message: "no such deal",
		}
	}

	return d.Response
}

func (sm *Miner) handleQueryDeal(s inet.Stream) {
	defer s.Close() // nolint: errcheck

	var q queryRequest
	if err := cbu.NewMsgReader(s).ReadMsg(&q); err != nil {
		log.Errorf("received invalid query: %s", err)
		return
	}

	ctx := context.Background()
	resp := sm.Query(ctx, q.Cid)

	if err := cbu.NewMsgWriter(s).WriteMsg(resp); err != nil {
		log.Errorf("failed to write query response: %s", err)
	}
}

func getFileSize(ctx context.Context, c cid.Cid, dserv ipld.DAGService) (uint64, error) {
	fnode, err := dserv.Get(ctx, c)
	if err != nil {
		return 0, err
	}
	switch n := fnode.(type) {
	case *dag.ProtoNode:
		return unixfs.DataSize(n.Data())
	case *dag.RawNode:
		return n.Size()
	default:
		return 0, fmt.Errorf("unrecognized node type: %T", fnode)
	}
}

func (sm *Miner) loadDeals() error {
	res, err := sm.dealsDs.Query(query.Query{
		Prefix: "/" + minerDatastorePrefix,
	})
	if err != nil {
		return errors.Wrap(err, "failed to query deals from datastore")
	}

	sm.deals = make(map[cid.Cid]*storageDeal)

	for entry := range res.Next() {
		var deal storageDeal
		if err := cbor.DecodeInto(entry.Value, &deal); err != nil {
			return errors.Wrap(err, "failed to unmarshal deals from datastore")
		}
		sm.deals[deal.Response.ProposalCid] = &deal
	}

	return nil
}

func (sm *Miner) saveDeal(proposalCid cid.Cid) error {
	marshalledDeal, err := cbor.DumpObject(sm.deals[proposalCid])
	if err != nil {
		return errors.Wrap(err, "Could not marshal storageDeal")
	}
	key := datastore.KeyWithNamespaces([]string{minerDatastorePrefix, proposalCid.String()})
	err = sm.dealsDs.Put(key, marshalledDeal)
	if err != nil {
		return errors.Wrap(err, "could not save client storage deal")
	}
	return nil
}
