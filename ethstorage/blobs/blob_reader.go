// Copyright 2022-2023, es.
// For license information, see https://github.com/ethstorage/es-node/blob/main/LICENSE

package blobs

import (
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	es "github.com/ethstorage/go-ethstorage/ethstorage"
	"github.com/ethstorage/go-ethstorage/ethstorage/downloader"
	"github.com/ethstorage/go-ethstorage/ethstorage/eth"
)

const (
	BlobReaderSubKey = "blob-reader"
)

// BlobReader provides unified interface for the miner to read blobs and samples
// from StorageManager and downloader cache.
type BlobReader struct {
	encodedBlobs sync.Map
	dlr          *downloader.Downloader
	sm           *es.StorageManager
	l1           *eth.PollingClient
	wg           sync.WaitGroup
	exitCh       chan struct{}
	lg           log.Logger
}

func NewBlobReader(dlr *downloader.Downloader, sm *es.StorageManager, l1 *eth.PollingClient, lg log.Logger) *BlobReader {
	n := &BlobReader{
		dlr:    dlr,
		sm:     sm,
		l1:     l1,
		lg:     lg,
		exitCh: make(chan struct{}),
	}
	n.sync()
	return n
}

// In order to provide miner with encoded samples in a timely manner,
// BlobReader is tracing the downloader and encoding newly cached blobs.
func (n *BlobReader) sync() {
	ch := make(chan common.Hash)
	downloader.SubscribeNewBlobs(BlobReaderSubKey, ch)
	go func() {
		defer func() {
			close(ch)
			downloader.Unsubscribe(BlobReaderSubKey)
			n.lg.Info("Blob reader unsubscribed downloader cache.")
			n.wg.Done()
		}()
		for {
			select {
			case blockHash := <-ch:
				for _, blob := range n.dlr.Cache.Blobs(blockHash) {
					encodedBlob := n.encodeBlob(blob)
					n.encodedBlobs.Store(blob.KvIdx(), encodedBlob)
				}
			case <-n.exitCh:
				n.lg.Info("Blob reader is exiting from downloader sync loop...")
				return
			}
		}
	}()
	n.wg.Add(1)
}

func (n *BlobReader) encodeBlob(blob downloader.Blob) []byte {
	shardIdx := blob.KvIdx() >> n.sm.KvEntriesBits()
	encodeType, _ := n.sm.GetShardEncodeType(shardIdx)
	miner, _ := n.sm.GetShardMiner(shardIdx)
	n.lg.Info("Encoding blob from downloader", "kvIdx", blob.KvIdx(), "shardIdx", shardIdx, "encodeType", encodeType, "miner", miner)
	encodeKey := es.CalcEncodeKey(blob.Hash(), blob.KvIdx(), miner)
	encodedBlob := es.EncodeChunk(blob.Size(), blob.Data(), encodeType, encodeKey)
	return encodedBlob
}

func (n *BlobReader) GetBlob(kvIdx uint64, kvHash common.Hash) ([]byte, error) {
	blob := n.dlr.Cache.GetKeyValueByIndex(kvIdx, kvHash)
	if blob != nil {
		n.lg.Debug("Loaded blob from downloader cache", "kvIdx", kvIdx)
		return blob, nil
	}
	blob, exist, err := n.sm.TryRead(kvIdx, int(n.sm.MaxKvSize()), kvHash)
	if err != nil {
		return nil, err
	}
	if !exist {
		return nil, fmt.Errorf("kv not found: index=%d", kvIdx)
	}
	n.lg.Debug("Loaded blob from storage manager", "kvIdx", kvIdx)
	return blob, nil
}

func (n *BlobReader) ReadSample(shardIdx, sampleIdx uint64) (common.Hash, error) {
	sampleLenBits := n.sm.MaxKvSizeBits() - es.SampleSizeBits
	kvIdx := sampleIdx >> sampleLenBits

	if value, ok := n.encodedBlobs.Load(kvIdx); ok {
		encodedBlob := value.([]byte)
		sampleIdxInKv := sampleIdx % (1 << sampleLenBits)
		sampleSize := uint64(1 << es.SampleSizeBits)
		sampleIdxByte := sampleIdxInKv << es.SampleSizeBits
		sample := encodedBlob[sampleIdxByte : sampleIdxByte+sampleSize]
		return common.BytesToHash(sample), nil
	}

	encodedSample, err := n.sm.ReadSampleUnlocked(shardIdx, sampleIdx)
	if err != nil {
		return common.Hash{}, err
	}
	return encodedSample, nil
}

func (n *BlobReader) Close() {
	n.lg.Info("Blob reader is being closed...")
	close(n.exitCh)
	n.wg.Wait()
}
