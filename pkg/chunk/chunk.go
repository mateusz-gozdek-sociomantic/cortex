package chunk

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
	"sync"

	prom_chunk "github.com/cortexproject/cortex/pkg/chunk/encoding"
	"github.com/cortexproject/cortex/pkg/prom1/storage/metric"
	"github.com/golang/snappy"
	jsoniter "github.com/json-iterator/go"
	ot "github.com/opentracing/opentracing-go"
	otlog "github.com/opentracing/opentracing-go/log"
	"github.com/pkg/errors"
	"github.com/prometheus/common/model"

	"github.com/cortexproject/cortex/pkg/util"
	errs "github.com/weaveworks/common/errors"
)

// Errors that decode can return
const (
	ErrInvalidChecksum = errs.Error("invalid chunk checksum")
	ErrWrongMetadata   = errs.Error("wrong chunk metadata")
	ErrMetadataLength  = errs.Error("chunk metadata wrong length")
	ErrDataLength      = errs.Error("chunk data wrong length")
)

var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

func errInvalidChunkID(s string) error {
	return errors.Errorf("invalid chunk ID %q", s)
}

// Chunk contains encoded timeseries data
type Chunk struct {
	// These two fields will be missing from older chunks (as will the hash).
	// On fetch we will initialise these fields from the DynamoDB key.
	Fingerprint model.Fingerprint `json:"fingerprint"`
	UserID      string            `json:"userID"`

	// These fields will be in all chunks, including old ones.
	From    model.Time   `json:"from"`
	Through model.Time   `json:"through"`
	Metric  model.Metric `json:"metric"`

	// The hash is not written to the external storage either.  We use
	// crc32, Castagnoli table.  See http://www.evanjones.ca/crc32c.html.
	// For old chunks, ChecksumSet will be false.
	ChecksumSet bool   `json:"-"`
	Checksum    uint32 `json:"-"`

	// We never use Delta encoding (the zero value), so if this entry is
	// missing, we default to DoubleDelta.
	Encoding prom_chunk.Encoding `json:"encoding"`
	Data     prom_chunk.Chunk    `json:"-"`

	// This flag is used for very old chunks, where the metadata is read out
	// of the index.
	metadataInIndex bool

	// The encoded version of the chunk, held so we don't need to re-encode it
	encoded []byte
}

// NewChunk creates a new chunk
func NewChunk(userID string, fp model.Fingerprint, metric model.Metric, c prom_chunk.Chunk, from, through model.Time) Chunk {
	return Chunk{
		Fingerprint: fp,
		UserID:      userID,
		From:        from,
		Through:     through,
		Metric:      metric,
		Encoding:    c.Encoding(),
		Data:        c,
	}
}

// ParseExternalKey is used to construct a partially-populated chunk from the
// key in DynamoDB.  This chunk can then be used to calculate the key needed
// to fetch the Chunk data from Memcache/S3, and then fully populate the chunk
// with decode().
//
// Pre-checksums, the keys written to DynamoDB looked like
// `<fingerprint>:<start time>:<end time>` (aka the ID), and the key for
// memcache and S3 was `<user id>/<fingerprint>:<start time>:<end time>.
// Finger prints and times were written in base-10.
//
// Post-checksums, externals keys become the same across DynamoDB, Memcache
// and S3.  Numbers become hex encoded.  Keys look like:
// `<user id>/<fingerprint>:<start time>:<end time>:<checksum>`.
func ParseExternalKey(userID, externalKey string) (Chunk, error) {
	if !strings.Contains(externalKey, "/") {
		return parseLegacyChunkID(userID, externalKey)
	}
	chunk, err := parseNewExternalKey(externalKey)
	if err != nil {
		return Chunk{}, err
	}
	if chunk.UserID != userID {
		return Chunk{}, errors.WithStack(ErrWrongMetadata)
	}
	return chunk, nil
}

func parseLegacyChunkID(userID, key string) (Chunk, error) {
	parts := strings.Split(key, ":")
	if len(parts) != 3 {
		return Chunk{}, errInvalidChunkID(key)
	}
	fingerprint, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return Chunk{}, err
	}
	from, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Chunk{}, err
	}
	through, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return Chunk{}, err
	}
	return Chunk{
		UserID:      userID,
		Fingerprint: model.Fingerprint(fingerprint),
		From:        model.Time(from),
		Through:     model.Time(through),
	}, nil
}

func parseNewExternalKey(key string) (Chunk, error) {
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		return Chunk{}, errInvalidChunkID(key)
	}
	userID := parts[0]
	hexParts := strings.Split(parts[1], ":")
	if len(hexParts) != 4 {
		return Chunk{}, errInvalidChunkID(key)
	}
	fingerprint, err := strconv.ParseUint(hexParts[0], 16, 64)
	if err != nil {
		return Chunk{}, err
	}
	from, err := strconv.ParseInt(hexParts[1], 16, 64)
	if err != nil {
		return Chunk{}, err
	}
	through, err := strconv.ParseInt(hexParts[2], 16, 64)
	if err != nil {
		return Chunk{}, err
	}
	checksum, err := strconv.ParseUint(hexParts[3], 16, 32)
	if err != nil {
		return Chunk{}, err
	}
	return Chunk{
		UserID:      userID,
		Fingerprint: model.Fingerprint(fingerprint),
		From:        model.Time(from),
		Through:     model.Time(through),
		Checksum:    uint32(checksum),
		ChecksumSet: true,
	}, nil
}

// ExternalKey returns the key you can use to fetch this chunk from external
// storage. For newer chunks, this key includes a checksum.
func (c *Chunk) ExternalKey() string {
	// Some chunks have a checksum stored in dynamodb, some do not.  We must
	// generate keys appropriately.
	if c.ChecksumSet {
		// This is the inverse of parseNewExternalKey.
		return fmt.Sprintf("%s/%x:%x:%x:%x", c.UserID, uint64(c.Fingerprint), int64(c.From), int64(c.Through), c.Checksum)
	}
	// This is the inverse of parseLegacyExternalKey, with "<user id>/" prepended.
	// Legacy chunks had the user ID prefix on s3/memcache, but not in DynamoDB.
	// See comment on parseExternalKey.
	return fmt.Sprintf("%s/%d:%d:%d", c.UserID, uint64(c.Fingerprint), int64(c.From), int64(c.Through))
}

var writerPool = sync.Pool{
	New: func() interface{} { return snappy.NewBufferedWriter(nil) },
}

// Encode writes the chunk out to a big write buffer, then calculates the checksum.
func (c *Chunk) Encode() ([]byte, error) {
	if c.encoded != nil {
		return c.encoded, nil
	}

	var buf bytes.Buffer

	// Write 4 empty bytes first - we will come back and put the len in here.
	metadataLenBytes := [4]byte{}
	if _, err := buf.Write(metadataLenBytes[:]); err != nil {
		return nil, err
	}

	// Encode chunk metadata into snappy-compressed buffer
	writer := writerPool.Get().(*snappy.Writer)
	defer writerPool.Put(writer)
	writer.Reset(&buf)
	json := jsoniter.ConfigFastest
	if err := json.NewEncoder(writer).Encode(c); err != nil {
		return nil, err
	}
	writer.Close()

	// Write the metadata length back at the start of the buffer.
	// (note this length includes the 4 bytes for the length itself)
	metadataLen := buf.Len()
	binary.BigEndian.PutUint32(metadataLenBytes[:], uint32(metadataLen))
	copy(buf.Bytes(), metadataLenBytes[:])

	// Write another 4 empty bytes - we will come back and put the len in here.
	dataLenBytes := [4]byte{}
	if _, err := buf.Write(dataLenBytes[:]); err != nil {
		return nil, err
	}

	// And now the chunk data
	if err := c.Data.Marshal(&buf); err != nil {
		return nil, err
	}

	// Now write the data len back into the buf.
	binary.BigEndian.PutUint32(dataLenBytes[:], uint32(buf.Len()-metadataLen-4))
	copy(buf.Bytes()[metadataLen:], dataLenBytes[:])

	// Now work out the checksum
	c.encoded = buf.Bytes()
	c.ChecksumSet = true
	c.Checksum = crc32.Checksum(c.encoded, castagnoliTable)
	return c.encoded, nil
}

// EncodedSize returns the number of bytes in the encoded data for this chunk
func (c *Chunk) EncodedSize() int {
	return len(c.encoded)
}

// DecodeContext holds data that can be re-used between decodes of different chunks
type DecodeContext struct {
	reader *snappy.Reader
}

// NewDecodeContext creates a new, blank, DecodeContext
func NewDecodeContext() *DecodeContext {
	return &DecodeContext{
		reader: snappy.NewReader(nil),
	}
}

// Decode the chunk from the given buffer, and confirm the chunk is the one we
// expected.
func (c *Chunk) Decode(decodeContext *DecodeContext, input []byte) error {
	// Legacy chunks were written with metadata in the index.
	if c.metadataInIndex {
		var err error
		c.Data, err = prom_chunk.NewForEncoding(prom_chunk.DoubleDelta)
		if err != nil {
			return err
		}
		c.encoded = input
		return c.Data.UnmarshalFromBuf(input)
	}

	// First, calculate the checksum of the chunk and confirm it matches
	// what we expected.
	if c.ChecksumSet && c.Checksum != crc32.Checksum(input, castagnoliTable) {
		return errors.WithStack(ErrInvalidChecksum)
	}

	// Now unmarshal the chunk metadata.
	r := bytes.NewReader(input)
	var metadataLen uint32
	if err := binary.Read(r, binary.BigEndian, &metadataLen); err != nil {
		return err
	}
	var tempMetadata Chunk
	decodeContext.reader.Reset(r)
	json := jsoniter.ConfigFastest
	err := json.NewDecoder(decodeContext.reader).Decode(&tempMetadata)
	if err != nil {
		return err
	}
	if len(input)-r.Len() != int(metadataLen) {
		return ErrMetadataLength
	}

	// Next, confirm the chunks matches what we expected.  Easiest way to do this
	// is to compare what the decoded data thinks its external ID would be, but
	// we don't write the checksum to s3, so we have to copy the checksum in.
	if c.ChecksumSet {
		tempMetadata.Checksum, tempMetadata.ChecksumSet = c.Checksum, c.ChecksumSet
		if !equalByKey(*c, tempMetadata) {
			return errors.WithStack(ErrWrongMetadata)
		}
	}
	*c = tempMetadata

	// Older chunks always used DoubleDelta and did not write Encoding
	// to JSON, so override if it has the zero value (Delta)
	if c.Encoding == prom_chunk.Delta {
		c.Encoding = prom_chunk.DoubleDelta
	}

	// Finally, unmarshal the actual chunk data.
	c.Data, err = prom_chunk.NewForEncoding(c.Encoding)
	if err != nil {
		return err
	}

	var dataLen uint32
	if err := binary.Read(r, binary.BigEndian, &dataLen); err != nil {
		return err
	}

	c.encoded = input
	remainingData := input[len(input)-r.Len():]
	if int(dataLen) != len(remainingData) {
		return ErrDataLength
	}

	return c.Data.UnmarshalFromBuf(remainingData[:int(dataLen)])
}

func equalByKey(a, b Chunk) bool {
	return a.UserID == b.UserID && a.Fingerprint == b.Fingerprint &&
		a.From == b.From && a.Through == b.Through && a.Checksum == b.Checksum
}

// ChunksToMatrix converts a set of chunks to a model.Matrix.
func ChunksToMatrix(ctx context.Context, chunks []Chunk, from, through model.Time) (model.Matrix, error) {
	sp, ctx := ot.StartSpanFromContext(ctx, "chunksToMatrix")
	defer sp.Finish()
	sp.LogFields(otlog.Int("chunks", len(chunks)))

	// Group chunks by series, sort and dedupe samples.
	metrics := map[model.Fingerprint]model.Metric{}
	samplesBySeries := map[model.Fingerprint][][]model.SamplePair{}
	for _, c := range chunks {
		ss, err := c.Samples(from, through)
		if err != nil {
			return nil, err
		}

		metrics[c.Fingerprint] = c.Metric
		samplesBySeries[c.Fingerprint] = append(samplesBySeries[c.Fingerprint], ss)
	}
	sp.LogFields(otlog.Int("series", len(samplesBySeries)))

	matrix := make(model.Matrix, 0, len(samplesBySeries))
	for fp, ss := range samplesBySeries {
		matrix = append(matrix, &model.SampleStream{
			Metric: metrics[fp],
			Values: util.MergeNSampleSets(ss...),
		})
	}

	return matrix, nil
}

// Samples returns all SamplePairs for the chunk.
func (c *Chunk) Samples(from, through model.Time) ([]model.SamplePair, error) {
	it := c.Data.NewIterator()
	interval := metric.Interval{OldestInclusive: from, NewestInclusive: through}
	return prom_chunk.RangeValues(it, interval)
}
