package sourceio

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sort"
	"sync"

	sessionio "github.com/nikitatsym/agent-session-io"
)

type recordIndex struct {
	record       uint64
	line         uint64
	start        int64
	dataEnd      int64
	end          int64
	digest       [sha256.Size]byte
	prefixDigest [sha256.Size]byte
}

type generationIndex struct {
	records                          []recordIndex
	tail                             TailState
	tailDigest                       [sha256.Size]byte
	generationDigest                 [sha256.Size]byte
	previousGenerationPrefixDigest   [sha256.Size]byte
	previousGenerationPrefixComplete bool
	confirmedPrefixDigest            [sha256.Size]byte
	confirmedPrefixComplete          bool
}

type prefixCapture struct {
	limit   int64
	written int64
	hash    hash.Hash
}

func newPrefixCapture(limit int64) *prefixCapture {
	return &prefixCapture{
		limit: limit,
		hash:  sha256.New(),
	}
}

func (capture *prefixCapture) write(data []byte) {
	if capture == nil || capture.written >= capture.limit {
		return
	}
	remaining := capture.limit - capture.written
	if int64(len(data)) > remaining {
		data = data[:remaining]
	}
	_, _ = capture.hash.Write(data)
	capture.written += int64(len(data))
}

func (capture *prefixCapture) result() ([sha256.Size]byte, bool) {
	if capture == nil {
		return [sha256.Size]byte{}, false
	}
	return hashDigest(capture.hash), capture.written == capture.limit
}

// JSONLGeneration is one indexed, bounded file view.
type JSONLGeneration struct {
	mu sync.Mutex

	file     *os.File
	spec     FileSpec
	identity FileIdentity
	size     int64
	revision sessionio.Revision
	records  []recordIndex
	tail     TailState
	tailHash [sha256.Size]byte

	nextIndex       int
	confirmedEnd    int64
	nextRecord      uint64
	nextLine        uint64
	confirmedPrefix [sha256.Size]byte

	tailVerified bool
	exhausted    bool
	terminalErr  error
	closed       bool
	closeResult  error
}

func openJSONLGeneration(
	ctx context.Context,
	spec FileSpec,
	options OpenOptions,
) (OpenResult, error) {
	if err := validateOpenRequest(ctx, spec, options); err != nil {
		return OpenResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return OpenResult{}, err
	}

	file, err := os.Open(spec.OpenPath)
	if options.Resume != nil && errors.Is(err, os.ErrNotExist) {
		return OpenResult{
			Reconciliation: Reconciliation{
				Change: FileChangeDisappeared,
				Resume: ResumeUnavailable,
			},
		}, nil
	}
	if err != nil {
		return OpenResult{}, fmt.Errorf("sourceio: open %q: %w", spec.OpenPath, err)
	}

	info, err := file.Stat()
	if err != nil {
		return OpenResult{}, closeAfterError(
			file,
			fmt.Errorf("sourceio: stat opened file %q: %w", spec.OpenPath, err),
		)
	}
	if !info.Mode().IsRegular() {
		return OpenResult{}, closeAfterError(
			file,
			fmt.Errorf("sourceio: path %q is not a regular file", spec.OpenPath),
		)
	}

	size := info.Size()
	index, err := indexGeneration(ctx, file, size, spec, options)
	if err != nil {
		return OpenResult{}, closeAfterError(file, err)
	}

	identity := FileIdentity{info: info}
	revision := revisionFromDigest(index.generationDigest)
	reconciliation := reconcile(
		options.Resume,
		identity,
		size,
		revision,
		index,
	)
	generation := &JSONLGeneration{
		file:            file,
		spec:            spec,
		identity:        identity,
		size:            size,
		revision:        revision,
		records:         index.records,
		tail:            cloneTail(index.tail),
		tailHash:        index.tailDigest,
		nextRecord:      1,
		nextLine:        1,
		confirmedPrefix: sha256.Sum256(nil),
	}

	if reconciliation.Resume == ResumeContinue {
		resume := options.Resume
		generation.confirmedEnd = resume.ConfirmedEnd
		generation.nextRecord = resume.NextRecord
		generation.nextLine = resume.NextLine
		generation.confirmedPrefix = resume.ConfirmedPrefixSHA256
		nextIndex, ok := findResumeIndex(
			index.records,
			index.tail,
			size,
			resume.ConfirmedEnd,
		)
		if !ok {
			return OpenResult{}, closeAfterError(
				file,
				fmt.Errorf(
					"sourceio: confirmed offset %d is not a JSONL record boundary",
					resume.ConfirmedEnd,
				),
			)
		}
		if !resumeNumberingMatches(
			index.records,
			nextIndex,
			resume.NextRecord,
			resume.NextLine,
		) {
			return OpenResult{}, closeAfterError(
				file,
				fmt.Errorf(
					"sourceio: resume numbering %d/%d does not match confirmed offset %d",
					resume.NextRecord,
					resume.NextLine,
					resume.ConfirmedEnd,
				),
			)
		}
		generation.nextIndex = nextIndex
	}

	return OpenResult{
		Generation:     generation,
		Reconciliation: reconciliation,
	}, nil
}

func validateOpenRequest(ctx context.Context, spec FileSpec, options OpenOptions) error {
	if ctx == nil {
		return errors.New("sourceio: context must not be nil")
	}
	if spec.OpenPath == "" {
		return errors.New("sourceio: open path must not be empty")
	}
	if spec.Locator.Root == "" {
		return errors.New("sourceio: locator root must not be empty")
	}
	if spec.Locator.Path == "" {
		return errors.New("sourceio: locator path must not be empty")
	}
	if spec.Locator.Record != nil ||
		spec.Locator.Line != nil ||
		spec.Locator.ByteRange != nil {
		return errors.New("sourceio: base file locator must not identify a record")
	}
	switch options.TailMode {
	case TailModeGrowing, TailModeFinal:
	default:
		return fmt.Errorf("sourceio: unsupported tail mode %q", options.TailMode)
	}
	if options.SizePolicy.MaxBytes != UnlimitedRecordBytes &&
		options.SizePolicy.MaxBytes <= 0 {
		return fmt.Errorf(
			"sourceio: record size limit must be positive or %d",
			UnlimitedRecordBytes,
		)
	}
	if options.Resume != nil {
		if err := validateResumeToken(*options.Resume); err != nil {
			return err
		}
	}
	return nil
}

func validateResumeToken(token ResumeToken) error {
	if token.Identity.info == nil {
		return errors.New("sourceio: resume token identity is missing")
	}
	if token.GenerationSize < 0 {
		return errors.New("sourceio: resume generation size must not be negative")
	}
	if token.ConfirmedEnd < 0 || token.ConfirmedEnd > token.GenerationSize {
		return fmt.Errorf(
			"sourceio: confirmed offset %d is outside generation size %d",
			token.ConfirmedEnd,
			token.GenerationSize,
		)
	}
	if token.NextRecord == 0 || token.NextLine == 0 {
		return errors.New("sourceio: resume record and line must be one-based")
	}
	if _, err := revisionDigest(token.GenerationRevision); err != nil {
		return err
	}
	return nil
}

func indexGeneration(
	ctx context.Context,
	file *os.File,
	size int64,
	spec FileSpec,
	options OpenOptions,
) (generationIndex, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return generationIndex{}, fmt.Errorf(
			"sourceio: seek %q for indexing: %w",
			spec.OpenPath,
			err,
		)
	}

	var previousCapture *prefixCapture
	var confirmedCapture *prefixCapture
	if options.Resume != nil {
		previousCapture = newPrefixCapture(options.Resume.GenerationSize)
		confirmedCapture = newPrefixCapture(options.Resume.ConfirmedEnd)
	}

	fullHash := sha256.New()
	reader := bufio.NewReader(io.LimitReader(file, size))
	result := generationIndex{
		tail: TailState{Kind: TailKindClean},
	}
	var candidate []byte
	var absolute int64
	var candidateStart int64
	recordNumber := uint64(1)
	lineNumber := uint64(1)

	for {
		if err := ctx.Err(); err != nil {
			return generationIndex{}, err
		}

		fragment, readErr := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			_, _ = fullHash.Write(fragment)
			previousCapture.write(fragment)
			confirmedCapture.write(fragment)
			absolute += int64(len(fragment))
			candidate = append(candidate, fragment...)
		}

		switch {
		case readErr == nil:
			record, err := indexTerminatedRecord(
				spec,
				options.SizePolicy,
				recordNumber,
				lineNumber,
				candidateStart,
				candidate,
				hashDigest(fullHash),
			)
			if err != nil {
				return generationIndex{}, err
			}
			result.records = append(result.records, record)
			candidate = nil
			candidateStart = absolute
			recordNumber++
			lineNumber++
		case errors.Is(readErr, bufio.ErrBufferFull):
			if err := enforceRecordSize(
				spec,
				options.SizePolicy,
				recordNumber,
				lineNumber,
				candidateStart,
				unterminatedDataLowerBound(candidate),
			); err != nil {
				return generationIndex{}, err
			}
		case errors.Is(readErr, io.EOF):
			if absolute != size {
				return generationIndex{}, fmt.Errorf(
					"sourceio: source size changed while indexing %q: expected=%d read=%d",
					spec.OpenPath,
					size,
					absolute,
				)
			}
			if len(candidate) > 0 {
				if err := enforceRecordSize(
					spec,
					options.SizePolicy,
					recordNumber,
					lineNumber,
					candidateStart,
					int64(len(candidate)),
				); err != nil {
					return generationIndex{}, err
				}
				if options.TailMode == TailModeFinal {
					record, err := indexFinalRecord(
						spec,
						recordNumber,
						lineNumber,
						candidateStart,
						candidate,
						hashDigest(fullHash),
					)
					if err != nil {
						return generationIndex{}, err
					}
					result.records = append(result.records, record)
				} else {
					byteRange := sessionio.ByteRange{
						Start: candidateStart,
						End:   absolute,
					}
					result.tail = TailState{
						Kind:      TailKindPending,
						ByteRange: &byteRange,
					}
					result.tailDigest = sha256.Sum256(candidate)
				}
			}
			result.generationDigest = hashDigest(fullHash)
			result.previousGenerationPrefixDigest,
				result.previousGenerationPrefixComplete = previousCapture.result()
			result.confirmedPrefixDigest,
				result.confirmedPrefixComplete = confirmedCapture.result()
			return result, nil
		default:
			return generationIndex{}, fmt.Errorf(
				"sourceio: read %q at byte %d: %w",
				spec.OpenPath,
				absolute,
				readErr,
			)
		}
	}
}

func indexTerminatedRecord(
	spec FileSpec,
	policy RecordSizePolicy,
	recordNumber uint64,
	lineNumber uint64,
	start int64,
	raw []byte,
	prefixDigest [sha256.Size]byte,
) (recordIndex, error) {
	framingLength := int64(1)
	if len(raw) >= 2 && raw[len(raw)-2] == '\r' {
		framingLength = 2
	}
	dataLength := int64(len(raw)) - framingLength
	if err := enforceRecordSize(
		spec,
		policy,
		recordNumber,
		lineNumber,
		start,
		dataLength,
	); err != nil {
		return recordIndex{}, err
	}
	data := raw[:dataLength]
	if err := validateJSONRecord(data); err != nil {
		record := recordForRange(
			recordNumber,
			lineNumber,
			start,
			start+dataLength,
		)
		return recordIndex{}, &MalformedJSONLError{
			Locator: record.SourceLocator(spec.Locator),
			Err:     err,
		}
	}
	return recordIndex{
		record:       recordNumber,
		line:         lineNumber,
		start:        start,
		dataEnd:      start + dataLength,
		end:          start + int64(len(raw)),
		digest:       sha256.Sum256(raw),
		prefixDigest: prefixDigest,
	}, nil
}

func indexFinalRecord(
	spec FileSpec,
	recordNumber uint64,
	lineNumber uint64,
	start int64,
	data []byte,
	prefixDigest [sha256.Size]byte,
) (recordIndex, error) {
	if err := validateJSONRecord(data); err != nil {
		record := recordForRange(
			recordNumber,
			lineNumber,
			start,
			start+int64(len(data)),
		)
		return recordIndex{}, &MalformedJSONLError{
			Locator: record.SourceLocator(spec.Locator),
			Err:     err,
		}
	}
	end := start + int64(len(data))
	return recordIndex{
		record:       recordNumber,
		line:         lineNumber,
		start:        start,
		dataEnd:      end,
		end:          end,
		digest:       sha256.Sum256(data),
		prefixDigest: prefixDigest,
	}, nil
}

func validateJSONRecord(data []byte) error {
	if json.Valid(data) {
		return nil
	}
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	return errors.New("record is not one complete JSON value")
}

func unterminatedDataLowerBound(candidate []byte) int64 {
	length := int64(len(candidate))
	if length > 0 && candidate[len(candidate)-1] == '\r' {
		return length - 1
	}
	return length
}

func enforceRecordSize(
	spec FileSpec,
	policy RecordSizePolicy,
	recordNumber uint64,
	lineNumber uint64,
	start int64,
	observed int64,
) error {
	if policy.MaxBytes == UnlimitedRecordBytes || observed <= policy.MaxBytes {
		return nil
	}
	record := recordForRange(
		recordNumber,
		lineNumber,
		start,
		start+observed,
	)
	return &RecordTooLargeError{
		Locator:         record.SourceLocator(spec.Locator),
		Limit:           policy.MaxBytes,
		ObservedAtLeast: observed,
	}
}

func reconcile(
	resume *ResumeToken,
	identity FileIdentity,
	size int64,
	revision sessionio.Revision,
	index generationIndex,
) Reconciliation {
	if resume == nil {
		return Reconciliation{
			Change: FileChangeInitial,
			Resume: ResumeReplay,
		}
	}

	var change FileChangeKind
	switch {
	case !sameIdentity(resume.Identity, identity):
		change = FileChangeReplaced
	case size < resume.GenerationSize:
		change = FileChangeTruncated
	case size == resume.GenerationSize && revision == resume.GenerationRevision:
		change = FileChangeUnchanged
	case size > resume.GenerationSize &&
		index.previousGenerationPrefixComplete &&
		revisionFromDigest(index.previousGenerationPrefixDigest) ==
			resume.GenerationRevision:
		change = FileChangeGrown
	default:
		change = FileChangeRewritten
	}

	action := ResumeReplay
	if index.confirmedPrefixComplete &&
		index.confirmedPrefixDigest == resume.ConfirmedPrefixSHA256 {
		action = ResumeContinue
	}
	return Reconciliation{
		Change: change,
		Resume: action,
	}
}

func findResumeIndex(
	records []recordIndex,
	tail TailState,
	size int64,
	confirmedEnd int64,
) (int, bool) {
	index := sort.Search(len(records), func(index int) bool {
		return records[index].start >= confirmedEnd
	})
	if index < len(records) && records[index].start == confirmedEnd {
		return index, true
	}
	if index == len(records) && confirmedEnd == size {
		return index, true
	}
	if index == len(records) &&
		tail.Kind == TailKindPending &&
		tail.ByteRange != nil &&
		tail.ByteRange.Start == confirmedEnd {
		return index, true
	}
	return 0, false
}

func resumeNumberingMatches(
	records []recordIndex,
	index int,
	nextRecord uint64,
	nextLine uint64,
) bool {
	if index < len(records) {
		return records[index].record == nextRecord &&
			records[index].line == nextLine
	}
	return nextRecord == uint64(len(records)+1) &&
		nextLine == uint64(len(records)+1)
}

// Revision returns the SHA-256 revision of the bounded generation.
func (generation *JSONLGeneration) Revision() sessionio.Revision {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	return generation.revision
}

// Next returns the next verified complete record.
func (generation *JSONLGeneration) Next(ctx context.Context) (JSONLRecord, error) {
	generation.mu.Lock()
	defer generation.mu.Unlock()

	if generation.closed {
		return JSONLRecord{}, sessionio.ErrStreamClosed
	}
	if generation.terminalErr != nil {
		return JSONLRecord{}, generation.terminalErr
	}
	if generation.exhausted {
		return JSONLRecord{}, io.EOF
	}
	if ctx == nil {
		return JSONLRecord{}, errors.New("sourceio: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return JSONLRecord{}, err
	}

	if generation.nextIndex < len(generation.records) {
		record, err := generation.readRecord(ctx, generation.records[generation.nextIndex])
		if err != nil {
			var changed *ChangedSourceError
			if errors.As(err, &changed) {
				generation.terminalErr = err
			}
			return JSONLRecord{}, err
		}
		indexed := generation.records[generation.nextIndex]
		generation.nextIndex++
		generation.confirmedEnd = indexed.end
		generation.nextRecord = indexed.record + 1
		generation.nextLine = indexed.line + 1
		generation.confirmedPrefix = indexed.prefixDigest
		return record, nil
	}

	if !generation.tailVerified {
		if err := generation.verifyTail(ctx); err != nil {
			var changed *ChangedSourceError
			if errors.As(err, &changed) {
				generation.terminalErr = err
			}
			return JSONLRecord{}, err
		}
		generation.tailVerified = true
	}
	generation.exhausted = true
	return JSONLRecord{}, io.EOF
}

func (generation *JSONLGeneration) readRecord(
	ctx context.Context,
	index recordIndex,
) (JSONLRecord, error) {
	rawLength := index.end - index.start
	raw := make([]byte, rawLength)
	count, err := generation.file.ReadAt(raw, index.start)
	if err != nil && !errors.Is(err, io.EOF) {
		return JSONLRecord{}, fmt.Errorf(
			"sourceio: read %q at byte %d: %w",
			generation.spec.OpenPath,
			index.start,
			err,
		)
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return JSONLRecord{}, contextErr
	}
	actual := sha256.Sum256(raw[:count])
	record := recordForRange(index.record, index.line, index.start, index.dataEnd)
	if int64(count) != rawLength || actual != index.digest {
		return JSONLRecord{}, &ChangedSourceError{
			Locator:        record.SourceLocator(generation.spec.Locator),
			ExpectedSHA256: index.digest,
			ActualSHA256:   actual,
			Resume:         generation.resumeToken(),
		}
	}
	dataLength := index.dataEnd - index.start
	return JSONLRecord{
		Record:    index.record,
		Line:      index.line,
		ByteRange: record.ByteRange,
		Data:      raw[:dataLength],
		Framing:   raw[dataLength:],
	}, nil
}

func (generation *JSONLGeneration) verifyTail(ctx context.Context) error {
	if generation.tail.Kind != TailKindPending || generation.tail.ByteRange == nil {
		return nil
	}
	byteRange := *generation.tail.ByteRange
	length := byteRange.End - byteRange.Start
	reader := io.NewSectionReader(generation.file, byteRange.Start, length)
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	var read int64
	for read < length {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, err := reader.Read(buffer)
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
			read += int64(count)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf(
				"sourceio: verify pending tail %q at byte %d: %w",
				generation.spec.OpenPath,
				byteRange.Start+read,
				err,
			)
		}
	}
	actual := hashDigest(hash)
	if read != length || actual != generation.tailHash {
		record := recordForRange(
			generation.nextRecord,
			generation.nextLine,
			byteRange.Start,
			byteRange.End,
		)
		return &ChangedSourceError{
			Locator:        record.SourceLocator(generation.spec.Locator),
			ExpectedSHA256: generation.tailHash,
			ActualSHA256:   actual,
			Resume:         generation.resumeToken(),
		}
	}
	return nil
}

// Tail returns a detached copy of the indexed tail state.
func (generation *JSONLGeneration) Tail() TailState {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	return cloneTail(generation.tail)
}

// ResumeToken returns the observed generation and last confirmed cursor.
func (generation *JSONLGeneration) ResumeToken() ResumeToken {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	return generation.resumeToken()
}

func (generation *JSONLGeneration) resumeToken() ResumeToken {
	return ResumeToken{
		Identity:              generation.identity,
		GenerationSize:        generation.size,
		GenerationRevision:    generation.revision,
		ConfirmedEnd:          generation.confirmedEnd,
		NextRecord:            generation.nextRecord,
		NextLine:              generation.nextLine,
		ConfirmedPrefixSHA256: generation.confirmedPrefix,
	}
}

// Close releases the source handle and returns a stable close result.
func (generation *JSONLGeneration) Close() error {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	if generation.closed {
		return generation.closeResult
	}
	generation.closed = true
	generation.closeResult = generation.file.Close()
	return generation.closeResult
}

func recordForRange(
	recordNumber uint64,
	lineNumber uint64,
	start int64,
	end int64,
) JSONLRecord {
	return JSONLRecord{
		Record: recordNumber,
		Line:   lineNumber,
		ByteRange: sessionio.ByteRange{
			Start: start,
			End:   end,
		},
	}
}

func cloneTail(tail TailState) TailState {
	cloned := TailState{Kind: tail.Kind}
	if tail.ByteRange != nil {
		byteRange := *tail.ByteRange
		cloned.ByteRange = &byteRange
	}
	return cloned
}

func hashDigest(hash hash.Hash) [sha256.Size]byte {
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

func closeAfterError(file *os.File, err error) error {
	if closeErr := file.Close(); closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return err
}
