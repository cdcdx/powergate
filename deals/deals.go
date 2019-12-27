package deals

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/address"
	"github.com/filecoin-project/lotus/chain/store"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	logging "github.com/ipfs/go-log"
	peer "github.com/libp2p/go-libp2p-core/peer"
)

const (
	initialWait        = time.Second * 5
	chanWriteTimeout   = time.Second
	askRefreshInterval = time.Second * 10
)

var (
	ErrCidIsNotImported = fmt.Errorf("data should be imported before staring a deal")

	log = logging.Logger("deals")
)

// DealerAPI interacts with a Filecoin full-node
type DealerAPI interface {
	ClientStartDeal(ctx context.Context, data cid.Cid, addr address.Address, miner address.Address, epochPrice types.BigInt, blocksDuration uint64) (*cid.Cid, error)
	ClientImport(ctx context.Context, path string) (cid.Cid, error)
	ClientGetDealInfo(context.Context, cid.Cid) (*api.DealInfo, error)
	ChainNotify(context.Context) (<-chan []*store.HeadChange, error)
	StateListMiners(context.Context, *types.TipSet) ([]address.Address, error)
	ClientQueryAsk(ctx context.Context, p peer.ID, miner address.Address) (*types.SignedStorageAsk, error)
	StateMinerPeerID(ctx context.Context, m address.Address, ts *types.TipSet) (peer.ID, error)
}

// DealModule exposes storage, monitoring, and Asks from the market.
type DealModule struct {
	api DealerAPI
	ds  datastore.Datastore

	askCacheLock sync.RWMutex
	askCache     []*types.StorageAsk

	lock        sync.Mutex
	stateClosed bool
	close       chan struct{}
	closed      chan struct{}
}

// DealConfig contains information about a proposal for a particular miner
type DealConfig struct {
	Miner      string
	EpochPrice types.BigInt
}

// DealInfo contains information about a proposal storage deal
type DealInfo struct {
	ProposalCid cid.Cid
	StateID     uint64
	StateName   string
	Miner       string

	PieceRef []byte
	Size     uint64

	PricePerEpoch types.BigInt
	Duration      uint64
}

// New creates a new deal module
func New(api DealerAPI, ds datastore.Datastore) *DealModule {
	dm := &DealModule{
		api:    api,
		ds:     ds,
		close:  make(chan struct{}),
		closed: make(chan struct{}),
	}
	go dm.runBackgroundAskCache()
	return dm
}

// Close closes the deal module
func (d *DealModule) Close() {
	d.lock.Lock()
	defer d.lock.Unlock()
	if d.stateClosed {
		return
	}
	close(d.close)
	<-d.closed
	d.stateClosed = true
}

// Store creates a proposal deal for data using wallet addr to all miners indicated
// by dealConfigs for duration epochs
func (d *DealModule) Store(ctx context.Context, addr string, data io.Reader, dealConfigs []DealConfig, duration uint64) ([]cid.Cid, []DealConfig, error) {
	tmpF, err := ioutil.TempFile("", "import-*")
	if err != nil {
		return nil, nil, fmt.Errorf("error when creating tmpfile: %s", err)
	}
	defer os.Remove(tmpF.Name())
	defer tmpF.Close()
	if _, err := io.Copy(tmpF, data); err != nil {
		return nil, nil, fmt.Errorf("error when copying data to tmpfile: %s", err)
	}
	dataCid, err := d.api.ClientImport(ctx, tmpF.Name())
	if err != nil {
		return nil, nil, fmt.Errorf("error when importing data: %s", err)
	}

	myAddr, err := address.NewFromString(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("wallet addr is invalid: %s", err)
	}
	var proposals []cid.Cid
	var failed []DealConfig
	for _, dconfig := range dealConfigs {
		minerAddr, err := address.NewFromString(dconfig.Miner)
		if err != nil {
			log.Errorf("miner addr is invalid %v: %s", dconfig, err)
			failed = append(failed, dconfig)
			continue
		}
		proposal, err := d.api.ClientStartDeal(ctx, dataCid, myAddr, minerAddr, dconfig.EpochPrice, duration)
		if err != nil {
			log.Errorf("error when starting deal with %v: %s", dconfig, err)
			failed = append(failed, dconfig)
			continue
		}
		proposals = append(proposals, *proposal)
	}
	return proposals, failed, nil
}

// Watch returnas a channel with state changes of indicated proposals
func (d *DealModule) Watch(ctx context.Context, proposals []cid.Cid) (<-chan DealInfo, error) {
	ch := make(chan DealInfo)
	w, err := d.api.ChainNotify(ctx)
	if err != nil {
		return nil, fmt.Errorf("error when listening to chain changes: %s", err)
	}
	go func() {
		defer close(ch)

		currentState := make(map[cid.Cid]api.DealInfo)
		tout := time.After(initialWait)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tout:
				if err := d.pushNewChanges(ctx, currentState, proposals, ch); err != nil {
					log.Errorf("error when pushing new proposal states: %s", err)
				}
			case <-w:
				if err := d.pushNewChanges(ctx, currentState, proposals, ch); err != nil {
					log.Errorf("error when pushing new proposal states: %s", err)
				}
			}
		}
	}()
	return ch, nil
}

func (d *DealModule) pushNewChanges(ctx context.Context, currState map[cid.Cid]api.DealInfo, proposals []cid.Cid, ch chan<- DealInfo) error {
	for _, pcid := range proposals {
		dinfo, err := d.api.ClientGetDealInfo(ctx, pcid)
		if err != nil {
			log.Errorf("error when getting deal proposal info %s: %s", pcid, err)
			continue
		}
		if !reflect.DeepEqual(currState[pcid], dinfo) {
			currState[pcid] = *dinfo
			newState := DealInfo{
				ProposalCid:   dinfo.ProposalCid,
				StateID:       dinfo.State,
				StateName:     api.DealStates[dinfo.State],
				Miner:         dinfo.Provider.String(),
				PieceRef:      dinfo.PieceRef,
				Size:          dinfo.Size,
				PricePerEpoch: dinfo.PricePerEpoch,
				Duration:      dinfo.Duration,
			}
			select {
			case <-ctx.Done():
				return nil
			case ch <- newState:
			case <-time.After(chanWriteTimeout):
				log.Warnf("dropping new state since chan is blocked")
			}
		}
	}
	return nil
}
