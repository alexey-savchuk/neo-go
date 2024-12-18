package blockfetcher

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nspcc-dev/neo-go/pkg/config"
	"github.com/nspcc-dev/neo-go/pkg/core/block"
	gio "github.com/nspcc-dev/neo-go/pkg/io"
	"github.com/nspcc-dev/neo-go/pkg/services/oracle/neofs"
	"github.com/nspcc-dev/neo-go/pkg/wallet"
	"github.com/nspcc-dev/neofs-sdk-go/client"
	"github.com/nspcc-dev/neofs-sdk-go/object"
	oid "github.com/nspcc-dev/neofs-sdk-go/object/id"
	"go.uber.org/zap"
)

const (
	// oidSize is the size of the object ID in NeoFS.
	oidSize = sha256.Size
	// defaultTimeout is the default timeout for NeoFS requests.
	defaultTimeout = 5 * time.Minute
	// defaultOIDBatchSize is the default number of OIDs to search and fetch at once.
	defaultOIDBatchSize = 8000
	// defaultDownloaderWorkersCount is the default number of workers downloading blocks.
	defaultDownloaderWorkersCount = 100
)

// Ledger is an interface to Blockchain sufficient for Service.
type Ledger interface {
	GetConfig() config.Blockchain
	BlockHeight() uint32
}

// Service is a service that fetches blocks from NeoFS.
type Service struct {
	// isActive denotes whether the service is working or in the process of shutdown.
	isActive          atomic.Bool
	log               *zap.Logger
	cfg               config.NeoFSBlockFetcher
	stateRootInHeader bool

	chain        Ledger
	client       *client.Client
	enqueueBlock func(*block.Block) error
	account      *wallet.Account

	oidsCh   chan oid.ID
	blocksCh chan *block.Block
	// wg is a wait group for block downloaders.
	wg sync.WaitGroup

	// Global context for download operations cancellation.
	ctx       context.Context
	ctxCancel context.CancelFunc

	// A set of routines managing graceful Service shutdown.
	quit                  chan bool
	quitOnce              sync.Once
	exiterToOIDDownloader chan struct{}
	exiterToShutdown      chan struct{}
	oidDownloaderToExiter chan struct{}
	blockQueuerToExiter   chan struct{}

	shutdownCallback func()
}

// New creates a new BlockFetcher Service.
func New(chain Ledger, cfg config.NeoFSBlockFetcher, logger *zap.Logger, putBlock func(*block.Block) error, shutdownCallback func()) (*Service, error) {
	var (
		account *wallet.Account
		err     error
	)

	if cfg.UnlockWallet.Path != "" {
		walletFromFile, err := wallet.NewWalletFromFile(cfg.UnlockWallet.Path)
		if err != nil {
			return &Service{}, err
		}
		for _, acc := range walletFromFile.Accounts {
			if err := acc.Decrypt(cfg.UnlockWallet.Password, walletFromFile.Scrypt); err == nil {
				account = acc
				break
			}
		}
		if account == nil {
			return &Service{}, errors.New("failed to decrypt any account in the wallet")
		}
	} else {
		account, err = wallet.NewAccount()
		if err != nil {
			return &Service{}, err
		}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.OIDBatchSize <= 0 {
		cfg.OIDBatchSize = defaultOIDBatchSize
	}
	if cfg.DownloaderWorkersCount <= 0 {
		cfg.DownloaderWorkersCount = defaultDownloaderWorkersCount
	}
	if len(cfg.Addresses) == 0 {
		return &Service{}, errors.New("no addresses provided")
	}
	return &Service{
		chain: chain,
		log:   logger,
		cfg:   cfg,

		enqueueBlock:      putBlock,
		account:           account,
		stateRootInHeader: chain.GetConfig().StateRootInHeader,
		shutdownCallback:  shutdownCallback,

		quit:                  make(chan bool),
		exiterToOIDDownloader: make(chan struct{}),
		exiterToShutdown:      make(chan struct{}),
		oidDownloaderToExiter: make(chan struct{}),
		blockQueuerToExiter:   make(chan struct{}),

		// Use buffer of two batch sizes to load OIDs in advance:
		//  * first full block of OIDs is processing by Downloader
		//  * second full block of OIDs is available to be fetched by Downloader immediately
		//  * third half-filled block of OIDs is being collected by OIDsFetcher.
		oidsCh: make(chan oid.ID, 2*cfg.OIDBatchSize),

		// Use buffer of a single OIDs batch size to provide smooth downloading and
		// avoid pauses during blockqueue insertion.
		blocksCh: make(chan *block.Block, cfg.OIDBatchSize),
	}, nil
}

// Start runs the NeoFS BlockFetcher service.
func (bfs *Service) Start() error {
	if !bfs.isActive.CompareAndSwap(false, true) {
		return nil
	}
	bfs.log.Info("starting NeoFS BlockFetcher service")

	var err error
	bfs.ctx, bfs.ctxCancel = context.WithCancel(context.Background())
	bfs.client, err = neofs.GetSDKClient(bfs.ctx, bfs.cfg.Addresses[0], 10*time.Minute)
	if err != nil {
		bfs.isActive.CompareAndSwap(true, false)
		return fmt.Errorf("create SDK client: %w", err)
	}

	// Start routine that manages Service shutdown process.
	go bfs.exiter()

	// Start OIDs downloader routine.
	go bfs.oidDownloader()

	// Start the set of blocks downloading routines.
	for range bfs.cfg.DownloaderWorkersCount {
		bfs.wg.Add(1)
		go bfs.blockDownloader()
	}

	// Start routine that puts blocks into bQueue.
	go bfs.blockQueuer()

	return nil
}

// oidDownloader runs the appropriate blocks OID fetching method based on the configuration.
func (bfs *Service) oidDownloader() {
	defer close(bfs.oidDownloaderToExiter)

	var err error
	if bfs.cfg.SkipIndexFilesSearch {
		err = bfs.fetchOIDsBySearch()
	} else {
		err = bfs.fetchOIDsFromIndexFiles()
	}
	var force bool
	if err != nil {
		bfs.log.Error("NeoFS BlockFetcher service: OID downloading routine failed", zap.Error(err))
		force = true
	}
	// Stop the service since there's nothing to do anymore.
	bfs.stopService(force)
}

// blockDownloader downloads the block from NeoFS and sends it to the blocks channel.
func (bfs *Service) blockDownloader() {
	defer bfs.wg.Done()

	for blkOid := range bfs.oidsCh {
		ctx, cancel := context.WithTimeout(bfs.ctx, bfs.cfg.Timeout)
		defer cancel()

		rc, err := bfs.objectGet(ctx, blkOid.String())
		if err != nil {
			if isContextCanceledErr(err) {
				return
			}
			bfs.log.Error("failed to objectGet block", zap.String("oid", blkOid.String()), zap.Error(err))
			bfs.stopService(true)
			return
		}

		b, err := bfs.readBlock(rc)
		if err != nil {
			if isContextCanceledErr(err) {
				return
			}
			bfs.log.Error("failed to decode block from stream", zap.String("oid", blkOid.String()), zap.Error(err))
			bfs.stopService(true)
			return
		}
		select {
		case <-bfs.ctx.Done():
			return
		case bfs.blocksCh <- b:
		}
	}
}

// blockQueuer puts the block into the bqueue.
func (bfs *Service) blockQueuer() {
	defer close(bfs.blockQueuerToExiter)

	for b := range bfs.blocksCh {
		select {
		case <-bfs.ctx.Done():
			return
		default:
			err := bfs.enqueueBlock(b)
			if err != nil {
				bfs.log.Error("failed to enqueue block", zap.Uint32("index", b.Index), zap.Error(err))
				bfs.stopService(true)
				return
			}
		}
	}
}

// fetchOIDsFromIndexFiles fetches block OIDs from NeoFS by searching index files first.
func (bfs *Service) fetchOIDsFromIndexFiles() error {
	h := bfs.chain.BlockHeight()
	startIndex := h / bfs.cfg.IndexFileSize
	skip := h % bfs.cfg.IndexFileSize

	for {
		select {
		case <-bfs.exiterToOIDDownloader:
			return nil
		default:
			prm := client.PrmObjectSearch{}
			filters := object.NewSearchFilters()
			filters.AddFilter(bfs.cfg.IndexFileAttribute, fmt.Sprintf("%d", startIndex), object.MatchStringEqual)
			prm.SetFilters(filters)

			ctx, cancel := context.WithTimeout(bfs.ctx, bfs.cfg.Timeout)
			blockOidsObject, err := bfs.objectSearch(ctx, prm)
			cancel()
			if err != nil {
				if isContextCanceledErr(err) {
					return nil
				}
				return fmt.Errorf("failed to find '%s' object with index %d: %w", bfs.cfg.IndexFileAttribute, startIndex, err)
			}
			if len(blockOidsObject) == 0 {
				bfs.log.Info(fmt.Sprintf("NeoFS BlockFetcher service: no '%s' object found with index %d, stopping", bfs.cfg.IndexFileAttribute, startIndex))
				return nil
			}

			blockCtx, blockCancel := context.WithTimeout(bfs.ctx, bfs.cfg.Timeout)
			defer blockCancel()
			oidsRC, err := bfs.objectGet(blockCtx, blockOidsObject[0].String())
			if err != nil {
				if isContextCanceledErr(err) {
					return nil
				}
				return fmt.Errorf("failed to fetch '%s' object with index %d: %w", bfs.cfg.IndexFileAttribute, startIndex, err)
			}

			err = bfs.streamBlockOIDs(oidsRC, int(skip))
			if err != nil {
				if isContextCanceledErr(err) {
					return nil
				}
				return fmt.Errorf("failed to stream block OIDs with index %d: %w", startIndex, err)
			}

			startIndex++
			skip = 0
		}
	}
}

// streamBlockOIDs reads block OIDs from the read closer and sends them to the OIDs channel.
func (bfs *Service) streamBlockOIDs(rc io.ReadCloser, skip int) error {
	defer rc.Close()
	oidBytes := make([]byte, oidSize)
	oidsProcessed := 0

	for {
		_, err := io.ReadFull(rc, oidBytes)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read OID: %w", err)
		}

		if oidsProcessed < skip {
			oidsProcessed++
			continue
		}

		var oidBlock oid.ID
		if err := oidBlock.Decode(oidBytes); err != nil {
			return fmt.Errorf("failed to decode OID: %w", err)
		}

		select {
		case <-bfs.exiterToOIDDownloader:
			return nil
		case bfs.oidsCh <- oidBlock:
		}

		oidsProcessed++
	}
	if oidsProcessed != int(bfs.cfg.IndexFileSize) {
		return fmt.Errorf("block OIDs count mismatch: expected %d, processed %d", bfs.cfg.IndexFileSize, oidsProcessed)
	}
	return nil
}

// fetchOIDsBySearch fetches block OIDs from NeoFS by searching through the Block objects.
func (bfs *Service) fetchOIDsBySearch() error {
	startIndex := bfs.chain.BlockHeight()
	batchSize := uint32(bfs.cfg.OIDBatchSize)

	for {
		select {
		case <-bfs.exiterToOIDDownloader:
			return nil
		default:
			prm := client.PrmObjectSearch{}
			filters := object.NewSearchFilters()
			filters.AddFilter(bfs.cfg.BlockAttribute, fmt.Sprintf("%d", startIndex), object.MatchNumGE)
			filters.AddFilter(bfs.cfg.BlockAttribute, fmt.Sprintf("%d", startIndex+batchSize-1), object.MatchNumLE)
			prm.SetFilters(filters)
			ctx, cancel := context.WithTimeout(bfs.ctx, bfs.cfg.Timeout)
			blockOids, err := bfs.objectSearch(ctx, prm)
			cancel()
			if err != nil {
				if isContextCanceledErr(err) {
					return nil
				}
				return err
			}

			if len(blockOids) == 0 {
				bfs.log.Info(fmt.Sprintf("NeoFS BlockFetcher service: no block found with index %d, stopping", startIndex))
				return nil
			}
			for _, oid := range blockOids {
				select {
				case <-bfs.exiterToOIDDownloader:
					return nil
				case bfs.oidsCh <- oid:
				}
			}
			startIndex += batchSize
		}
	}
}

// readBlock decodes the block from the read closer and prepares it for adding to the blockchain.
func (bfs *Service) readBlock(rc io.ReadCloser) (*block.Block, error) {
	b := block.New(bfs.stateRootInHeader)
	r := gio.NewBinReaderFromIO(rc)
	b.DecodeBinary(r)
	rc.Close()
	return b, r.Err
}

// Shutdown stops the NeoFS BlockFetcher service. It prevents service from new
// block OIDs search, cancels all in-progress downloading operations and waits
// until all service routines finish their work.
func (bfs *Service) Shutdown() {
	if !bfs.IsActive() {
		return
	}
	bfs.stopService(true)
	<-bfs.exiterToShutdown
}

// stopService close quitting goroutine once. It's the only entrypoint to shutdown
// procedure.
func (bfs *Service) stopService(force bool) {
	bfs.quitOnce.Do(func() {
		bfs.quit <- force
		close(bfs.quit)
	})
}

// exiter is a routine that is listening to a quitting signal and manages graceful
// Service shutdown process.
func (bfs *Service) exiter() {
	// Closing signal may come from anyone, but only once.
	force := <-bfs.quit
	bfs.log.Info("shutting down NeoFS BlockFetcher service",
		zap.Bool("force", force),
	)

	// Cansel all pending OIDs/blocks downloads in case if shutdown requested by user
	// or caused by downloading error.
	if force {
		bfs.ctxCancel()
	}

	// Send signal to OID downloader to stop. Wait until OID downloader finishes his
	// work.
	close(bfs.exiterToOIDDownloader)
	<-bfs.oidDownloaderToExiter

	// Close OIDs channel to let block downloaders know that there are no more OIDs
	// expected. Wait until all downloaders finish their work.
	close(bfs.oidsCh)
	bfs.wg.Wait()

	// Send signal to block putter to finish his work. Wait until it's finished.
	close(bfs.blocksCh)
	<-bfs.blockQueuerToExiter

	// Everything is done, release resources, turn off the activity marker and let
	// the server know about it.
	_ = bfs.client.Close()
	_ = bfs.log.Sync()
	bfs.isActive.CompareAndSwap(true, false)
	bfs.shutdownCallback()

	// Notify Shutdown routine in case if it's user-triggered shutdown.
	close(bfs.exiterToShutdown)
}

// IsActive returns true if the NeoFS BlockFetcher service is running.
func (bfs *Service) IsActive() bool {
	return bfs.isActive.Load()
}

func (bfs *Service) objectGet(ctx context.Context, oid string) (io.ReadCloser, error) {
	u, err := url.Parse(fmt.Sprintf("neofs:%s/%s", bfs.cfg.ContainerID, oid))
	if err != nil {
		return nil, err
	}
	rc, err := neofs.GetWithClient(ctx, bfs.client, bfs.account.PrivateKey(), u, false)
	if err != nil {
		return nil, err
	}

	return rc, nil
}

func (bfs *Service) objectSearch(ctx context.Context, prm client.PrmObjectSearch) ([]oid.ID, error) {
	return neofs.ObjectSearch(ctx, bfs.client, bfs.account.PrivateKey(), bfs.cfg.ContainerID, prm)
}

// isContextCanceledErr returns whether error is a wrapped [context.Canceled].
// Ref. https://github.com/nspcc-dev/neofs-sdk-go/issues/624.
func isContextCanceledErr(err error) bool {
	return errors.Is(err, context.Canceled) ||
		strings.Contains(err.Error(), "context canceled")
}
