// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.
package core

import (
	crand "crypto/rand"
	"math"
	"math/big"
	mrand "math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/hashicorp/golang-lru"
)

// HeaderChain implements the basic block header chain logic that is shared by
// core.BlockChain and light.LightChain.
// It is not thread safe, the encapsulating chain structures should do the
// necessary mutex locking/unlocking. It needs a reference to the parent's
// interrupt semaphore and wait group to properly handle shutdown.
type HeaderChain struct {
	chainDb       ethdb.Database
	genesisHeader *types.Header

	currentHeader *types.Header // Current head of the header chain (may be above the block chain!)
	headerCache   *lru.Cache    // Cache for the most recent block headers
	tdCache       *lru.Cache    // Cache for the most recent block total difficulties

	// procInterrupt must be atomically called
	procInterrupt *int32 // interrupt signaler for header processing
	wg            *sync.WaitGroup

	rand         *mrand.Rand
	getValidator getHeaderValidatorFn
}

// getHeaderValidatorFn returns a HeaderValidator interface
type getHeaderValidatorFn func() HeaderValidator

// NewHeaderChain creates a new HeaderChain structure.
//  getValidator should return the parent's validator
//  procInterrupt points to the parent's interrupt semaphore
//  wg points to the parent's shutdown wait group
func NewHeaderChain(chainDb ethdb.Database, getValidator getHeaderValidatorFn, procInterrupt *int32, wg *sync.WaitGroup) (*HeaderChain, error) {
	headerCache, _ := lru.New(headerCacheLimit)
	tdCache, _ := lru.New(tdCacheLimit)

	// Seed a fast but crypto originating random generator
	seed, err := crand.Int(crand.Reader, big.NewInt(math.MaxInt64))
	if err != nil {
		return nil, err
	}

	hc := &HeaderChain{
		chainDb:       chainDb,
		headerCache:   headerCache,
		tdCache:       tdCache,
		procInterrupt: procInterrupt,
		wg:            wg,
		rand:          mrand.New(mrand.NewSource(seed.Int64())),
		getValidator:  getValidator,
	}

	hc.genesisHeader = hc.GetHeaderByNumber(0)
	if hc.genesisHeader == nil {
		genesisBlock, err := WriteDefaultGenesisBlock(chainDb)
		if err != nil {
			return nil, err
		}
		glog.V(logger.Info).Infoln("WARNING: Wrote default ethereum genesis block")
		hc.genesisHeader = genesisBlock.Header()
	}

	hc.currentHeader = hc.genesisHeader
	if head := GetHeadBlockHash(chainDb); head != (common.Hash{}) {
		if chead := hc.GetHeader(head); chead != nil {
			hc.currentHeader = chead
		}
	}

	return hc, nil
}

// writeHeader writes a header into the local chain, given that its parent is
// already known. If the total difficulty of the newly inserted header becomes
// greater than the current known TD, the canonical chain is re-routed.
//
// Note: This method is not concurrent-safe with inserting blocks simultaneously
// into the chain, as side effects caused by reorganisations cannot be emulated
// without the real blocks. Hence, writing headers directly should only be done
// in two scenarios: pure-header mode of operation (light clients), or properly
// separated header/block phases (non-archive clients).
func (self *HeaderChain) writeHeader(header *types.Header) error {
	self.wg.Add(1)
	defer self.wg.Done()

	// Calculate the total difficulty of the header
	ptd := self.GetTd(header.ParentHash)
	if ptd == nil {
		return ParentError(header.ParentHash)
	}
	td := new(big.Int).Add(header.Difficulty, ptd)

	// Make sure no inconsistent state is leaked during insertion

	// If the total difficulty is higher than our known, add it to the canonical chain
	if td.Cmp(self.GetTd(self.currentHeader.Hash())) > 0 {
		// Delete any canonical number assignments above the new head
		for i := header.Number.Uint64() + 1; GetCanonicalHash(self.chainDb, i) != (common.Hash{}); i++ {
			DeleteCanonicalHash(self.chainDb, i)
		}
		// Overwrite any stale canonical number assignments
		head := self.GetHeader(header.ParentHash)
		for GetCanonicalHash(self.chainDb, head.Number.Uint64()) != head.Hash() {
			WriteCanonicalHash(self.chainDb, head.Hash(), head.Number.Uint64())
			head = self.GetHeader(head.ParentHash)
		}
		// Extend the canonical chain with the new header
		if err := WriteCanonicalHash(self.chainDb, header.Hash(), header.Number.Uint64()); err != nil {
			glog.Fatalf("failed to insert header number: %v", err)
		}
		if err := WriteHeadHeaderHash(self.chainDb, header.Hash()); err != nil {
			glog.Fatalf("failed to insert head header hash: %v", err)
		}
		self.currentHeader = types.CopyHeader(header)
	}
	// Irrelevant of the canonical status, write the header itself to the database
	if err := WriteTd(self.chainDb, header.Hash(), td); err != nil {
		glog.Fatalf("failed to write header total difficulty: %v", err)
	}
	if err := WriteHeader(self.chainDb, header); err != nil {
		glog.Fatalf("filed to write header contents: %v", err)
	}
	return nil
}

// InsertHeaderChain attempts to insert the given header chain in to the local
// chain, possibly creating a reorg. If an error is returned, it will return the
// index number of the failing header as well an error describing what went wrong.
//
// The verify parameter can be used to fine tune whether nonce verification
// should be done or not. The reason behind the optional check is because some
// of the header retrieval mechanisms already need to verfy nonces, as well as
// because nonces can be verified sparsely, not needing to check each.
func (self *HeaderChain) InsertHeaderChain(chain []*types.Header, checkFreq int) (int, error) {
	self.wg.Add(1)
	defer self.wg.Done()

	// Collect some import statistics to report on
	stats := struct{ processed, ignored int }{}
	start := time.Now()

	// Generate the list of headers that should be POW verified
	verify := make([]bool, len(chain))
	for i := 0; i < len(verify)/checkFreq; i++ {
		index := i*checkFreq + self.rand.Intn(checkFreq)
		if index >= len(verify) {
			index = len(verify) - 1
		}
		verify[index] = true
	}
	verify[len(verify)-1] = true // Last should always be verified to avoid junk

	// Create the header verification task queue and worker functions
	tasks := make(chan int, len(chain))
	for i := 0; i < len(chain); i++ {
		tasks <- i
	}
	close(tasks)

	errs, failed := make([]error, len(tasks)), int32(0)
	process := func(worker int) {
		for index := range tasks {
			header, hash := chain[index], chain[index].Hash()

			// Short circuit insertion if shutting down or processing failed
			if atomic.LoadInt32(self.procInterrupt) == 1 {
				return
			}
			if atomic.LoadInt32(&failed) > 0 {
				return
			}
			// Short circuit if the header is bad or already known
			if BadHashes[hash] {
				errs[index] = BadHashError(hash)
				atomic.AddInt32(&failed, 1)
				return
			}
			if self.HasHeader(hash) {
				continue
			}
			// Verify that the header honors the chain parameters
			checkPow := verify[index]

			var err error
			if index == 0 {
				err = self.getValidator().ValidateHeader(header, self.GetHeader(header.ParentHash), checkPow)
			} else {
				err = self.getValidator().ValidateHeader(header, chain[index-1], checkPow)
			}
			if err != nil {
				errs[index] = err
				atomic.AddInt32(&failed, 1)
				return
			}
		}
	}
	// Start as many worker threads as goroutines allowed
	pending := new(sync.WaitGroup)
	for i := 0; i < runtime.GOMAXPROCS(0); i++ {
		pending.Add(1)
		go func(id int) {
			defer pending.Done()
			process(id)
		}(i)
	}
	pending.Wait()

	// If anything failed, report
	if failed > 0 {
		for i, err := range errs {
			if err != nil {
				return i, err
			}
		}
	}
	// All headers passed verification, import them into the database
	for i, header := range chain {
		// Short circuit insertion if shutting down
		if atomic.LoadInt32(self.procInterrupt) == 1 {
			glog.V(logger.Debug).Infoln("premature abort during header chain processing")
			break
		}
		hash := header.Hash()

		// If the header's already known, skip it, otherwise store
		if self.HasHeader(hash) {
			stats.ignored++
			continue
		}
		if err := self.writeHeader(header); err != nil {
			return i, err
		}
		stats.processed++
	}
	// Report some public statistics so the user has a clue what's going on
	first, last := chain[0], chain[len(chain)-1]
	glog.V(logger.Info).Infof("imported %d header(s) (%d ignored) in %v. #%v [%x… / %x…]", stats.processed, stats.ignored,
		time.Since(start), last.Number, first.Hash().Bytes()[:4], last.Hash().Bytes()[:4])

	return 0, nil
}

// GetBlockHashesFromHash retrieves a number of block hashes starting at a given
// hash, fetching towards the genesis block.
func (self *HeaderChain) GetBlockHashesFromHash(hash common.Hash, max uint64) []common.Hash {
	// Get the origin header from which to fetch
	header := self.GetHeader(hash)
	if header == nil {
		return nil
	}
	// Iterate the headers until enough is collected or the genesis reached
	chain := make([]common.Hash, 0, max)
	for i := uint64(0); i < max; i++ {
		if header = self.GetHeader(header.ParentHash); header == nil {
			break
		}
		chain = append(chain, header.Hash())
		if header.Number.Cmp(common.Big0) == 0 {
			break
		}
	}
	return chain
}

// GetTd retrieves a block's total difficulty in the canonical chain from the
// database by hash, caching it if found.
func (self *HeaderChain) GetTd(hash common.Hash) *big.Int {
	// Short circuit if the td's already in the cache, retrieve otherwise
	if cached, ok := self.tdCache.Get(hash); ok {
		return cached.(*big.Int)
	}
	td := GetTd(self.chainDb, hash)
	if td == nil {
		return nil
	}
	// Cache the found body for next time and return
	self.tdCache.Add(hash, td)
	return td
}

// GetHeader retrieves a block header from the database by hash, caching it if
// found.
func (self *HeaderChain) GetHeader(hash common.Hash) *types.Header {
	// Short circuit if the header's already in the cache, retrieve otherwise
	if header, ok := self.headerCache.Get(hash); ok {
		return header.(*types.Header)
	}
	header := GetHeader(self.chainDb, hash)
	if header == nil {
		return nil
	}
	// Cache the found header for next time and return
	self.headerCache.Add(header.Hash(), header)
	return header
}

// HasHeader checks if a block header is present in the database or not, caching
// it if present.
func (bc *HeaderChain) HasHeader(hash common.Hash) bool {
	return bc.GetHeader(hash) != nil
}

// GetHeaderByNumber retrieves a block header from the database by number,
// caching it (associated with its hash) if found.
func (self *HeaderChain) GetHeaderByNumber(number uint64) *types.Header {
	hash := GetCanonicalHash(self.chainDb, number)
	if hash == (common.Hash{}) {
		return nil
	}
	return self.GetHeader(hash)
}

// CurrentHeader retrieves the current head header of the canonical chain. The
// header is retrieved from the HeaderChain's internal cache.
func (self *HeaderChain) CurrentHeader() *types.Header {
	return self.currentHeader
}

// SetCurrentHeader sets the current head header of the canonical chain.
func (self *HeaderChain) SetCurrentHeader(head *types.Header) {
	if err := WriteHeadHeaderHash(self.chainDb, head.Hash()); err != nil {
		glog.Fatalf("failed to insert head header hash: %v", err)
	}
	self.currentHeader = head
}

// SetHead rewinds the local chain to a new head. Everything above the new head
// will be deleted and the new one set.
func (bc *HeaderChain) SetHead(head uint64) {
	height := uint64(0)
	if bc.currentHeader != nil {
		height = bc.currentHeader.Number.Uint64()
	}

	for bc.currentHeader != nil && bc.currentHeader.Number.Uint64() > head {
		hash := bc.currentHeader.Hash()
		DeleteHeader(bc.chainDb, hash)
		DeleteTd(bc.chainDb, hash)
		bc.currentHeader = bc.GetHeader(bc.currentHeader.ParentHash)
	}
	// Roll back the canonical chain numbering
	for i := height; i > head; i-- {
		DeleteCanonicalHash(bc.chainDb, i)
	}
	// Clear out any stale content from the caches
	bc.headerCache.Purge()
	bc.tdCache.Purge()

	if bc.currentHeader == nil {
		bc.currentHeader = bc.genesisHeader
	}
	if err := WriteHeadHeaderHash(bc.chainDb, bc.currentHeader.Hash()); err != nil {
		glog.Fatalf("failed to reset head header hash: %v", err)
	}
}

// Rollback is designed to remove a chain of links from the database that aren't
// certain enough to be valid.
func (self *HeaderChain) Rollback(chain []common.Hash) {
	for i := len(chain) - 1; i >= 0; i-- {
		hash := chain[i]

		if self.currentHeader.Hash() == hash {
			self.currentHeader = self.GetHeader(self.currentHeader.ParentHash)
			WriteHeadHeaderHash(self.chainDb, self.currentHeader.Hash())
		}
	}
}

// SetGenesis sets a new genesis block header for the chain
func (self *HeaderChain) SetGenesis(head *types.Header) {
	self.genesisHeader = head
}