package tsi1

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"io/ioutil"
	"os"
	"sort"

	"github.com/influxdata/influxdb/influxql"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/pkg/mmap"
	"github.com/influxdata/influxdb/tsdb"
)

// Log entry flag constants.
const (
	LogEntrySeriesTombstoneFlag      = 0x01
	LogEntryMeasurementTombstoneFlag = 0x02
	LogEntryTagKeyTombstoneFlag      = 0x04
	LogEntryTagValueTombstoneFlag    = 0x08
)

// LogFile represents an on-disk write-ahead log file.
type LogFile struct {
	data    []byte     // mmap
	file    *os.File   // writer
	buf     []byte     // marshaling buffer
	entries []LogEntry // parsed entries

	// In-memory index.
	series logSeries
	mms    logMeasurements

	// Filepath to the log file.
	Path string
}

// NewLogFile returns a new instance of LogFile.
func NewLogFile() *LogFile {
	return &LogFile{
		mms: make(logMeasurements),
	}
}

// Open reads the log from a file and validates all the checksums.
func (f *LogFile) Open() error {
	if err := f.open(); err != nil {
		f.Close()
		return err
	}
	return nil
}

func (f *LogFile) open() error {
	// Open file for appending.
	file, err := os.OpenFile(f.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	f.file = file

	// Finish opening if file is empty.
	fi, err := file.Stat()
	if err != nil {
		return err
	} else if fi.Size() == 0 {
		return nil
	}
	println("FI", fi.Size())

	buf, _ := ioutil.ReadFile(f.Path)
	Hexdump(buf)

	// Open a read-only memory map of the existing data.
	data, err := mmap.Map(f.Path)
	if err != nil {
		return err
	}
	f.data = data

	// Read log entries from mmap.
	f.entries = nil
	for buf := f.data; len(buf) > 0; {
		// Read next entry.
		var e LogEntry
		if err := e.UnmarshalBinary(buf); err != nil {
			return err
		}
		f.entries = append(f.entries, e)

		// Move buffer forward.
		buf = buf[e.Size:]
	}

	return nil
}

// Close shuts down the file handle and mmap.
func (f *LogFile) Close() error {
	if f.file != nil {
		f.file.Close()
		f.file = nil
	}
	if f.data != nil {
		mmap.Unmap(f.data)
	}
	f.entries = nil
	return nil
}

func (f *LogFile) CreateMeasurementIndexIfNotExists(name []byte) (*tsdb.Measurement, error) {
	panic("TODO")
}

// DeleteMeasurement adds a tombstone for a measurement to the log file.
func (f *LogFile) DeleteMeasurement(name []byte) error {
	// Append log entry.
	if err := f.append(LogEntry{Flag: LogEntryMeasurementTombstoneFlag, Name: name}); err != nil {
		return err
	}

	// Delete measurement from index.
	mm := f.measurement(name)
	mm.deleted = true
	f.mms[string(name)] = mm

	return nil
}

// DeleteTagKey adds a tombstone for a tag key to the log file.
func (f *LogFile) DeleteTagKey(name, key []byte) error {
	return f.append(LogEntry{Flag: LogEntryTagKeyTombstoneFlag, Name: name, Tags: models.Tags{{Key: key}}})
}

// DeleteTagValue adds a tombstone for a tag value to the log file.
func (f *LogFile) DeleteTagValue(name, key, value []byte) error {
	return f.append(LogEntry{Flag: LogEntryTagValueTombstoneFlag, Name: name, Tags: models.Tags{{Key: key, Value: value}}})
}

// Series returns a series reference.
func (f *LogFile) Series(name []byte, tags models.Tags) SeriesElem {
	mm, ok := f.mms[string(name)]
	if !ok {
		return nil
	}

	// Find index of series in measurement.
	i := sort.Search(len(mm.series), func(i int) bool {
		return models.CompareTags(mm.series[i].tags, tags) != -1
	})

	// Return if match found. Otherwise return nil.
	if i < len(mm.series) && mm.series[i].tags.Equal(tags) {
		e := mm.series[i]
		return &e
	}
	return nil
}

// AddSeries adds a series to the log file.
func (f *LogFile) AddSeries(name []byte, tags models.Tags) error {
	return f.insertSeries(LogEntry{Name: name, Tags: tags})
}

// DeleteSeries adds a tombstone for a series to the log file.
func (f *LogFile) DeleteSeries(name []byte, tags models.Tags) error {
	return f.insertSeries(LogEntry{Flag: LogEntrySeriesTombstoneFlag, Name: name, Tags: tags})
}

// insertSeries inserts a series entry.
func (f *LogFile) insertSeries(e LogEntry) error {
	// Append log entry.
	if err := f.append(e); err != nil {
		return err
	}

	// Check if series is deleted.
	deleted := (e.Flag & LogEntrySeriesTombstoneFlag) != 0

	// Insert series to list.
	f.series.insert(e.Name, e.Tags, deleted)

	// Fetch measurement.
	mm := f.measurement(e.Name)

	// Save tags.
	for _, t := range e.Tags {
		// Fetch key.
		ts, ok := mm.tagSet[string(t.Key)]
		if !ok {
			ts = logTagSet{name: t.Key, tagValues: make(map[string]logTagValue)}
		}

		// Fetch value.
		tv, ok := ts.tagValues[string(t.Value)]
		if !ok {
			tv.name = t.Value
		}
		tv.insertEntry(e)
		ts.tagValues[string(t.Value)] = tv

		// Save key.
		mm.tagSet[string(t.Key)] = ts
	}

	// Insert series to list.
	// TODO: Remove global series list.
	mm.series.insert(e.Name, e.Tags, deleted)

	// Save measurement.
	f.mms[string(e.Name)] = mm

	return nil
}

// append adds a generic entry to the end of the file.
func (f *LogFile) append(e LogEntry) error {
	// Marshal entry to the local buffer.
	f.buf = appendLogEntry(f.buf[0:], &e)

	// Append checksum.
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], e.Checksum)
	f.buf = append(f.buf, buf[:]...)

	// Save the size of the record.
	e.Size = len(f.buf)

	// Write record to file.
	if n, err := f.file.Write(f.buf); err != nil {
		// Move position backwards over partial entry.
		// Log should be reopened if seeking cannot be completed.
		if n > 0 {
			if _, err := f.file.Seek(int64(-n), os.SEEK_CUR); err != nil {
				f.Close()
			}
		}
		return err
	}

	// Save entry to in-memory list.
	f.entries = append(f.entries, e)

	return nil
}

// measurement returns a measurement by name.
func (f *LogFile) measurement(name []byte) logMeasurement {
	mm, ok := f.mms[string(name)]
	if !ok {
		mm = logMeasurement{name: name, tagSet: make(map[string]logTagSet)}
	}
	return mm
}

// MeasurementIterator returns an iterator over all the measurements in the file.
func (f *LogFile) MeasurementIterator() MeasurementIterator {
	var itr logMeasurementIterator
	for _, mm := range f.mms {
		itr.mms = append(itr.mms, mm)
	}
	sort.Sort(logMeasurementSlice(itr.mms))
	return &itr
}

// MeasurementSeriesIterator returns an iterator over all series in the log file.
func (f *LogFile) MeasurementSeriesIterator(name []byte) SeriesIterator {
	mm := f.mms[string(name)]
	itr := logSeriesIterator{series: make(logSeries, len(mm.series))}
	copy(itr.series, mm.series)
	return &itr
}

// CompactTo compacts the log file and writes it to w.
func (f *LogFile) CompactTo(w io.Writer) (n int64, err error) {
	var t IndexFileTrailer

	// Reset compaction fields.
	f.reset()

	// Write magic number.
	if err := writeTo(w, []byte(FileSignature), &n); err != nil {
		return n, err
	}

	// Write series list.
	t.SeriesBlock.Offset = n
	if err := f.writeSeriesBlockTo(w, &n); err != nil {
		return n, err
	}
	t.SeriesBlock.Size = n - t.SeriesBlock.Offset

	// Sort measurement names.
	names := f.mms.names()

	// Write tagset blocks in measurement order.
	if err := f.writeTagsetsTo(w, names, &n); err != nil {
		return n, err
	}

	// Write measurement block.
	t.MeasurementBlock.Offset = n
	if err := f.writeMeasurementBlockTo(w, names, &n); err != nil {
		return n, err
	}
	t.MeasurementBlock.Size = n - t.MeasurementBlock.Offset

	// Write trailer.
	nn, err := t.WriteTo(w)
	n += nn
	if err != nil {
		return n, err
	}

	return n, nil
}

func (f *LogFile) writeSeriesBlockTo(w io.Writer, n *int64) error {
	// Ensure series are sorted.
	sort.Sort(f.series)

	// Write all series.
	sw := NewSeriesBlockWriter()
	for _, serie := range f.series {
		if err := sw.Add(serie.name, serie.tags); err != nil {
			return err
		}
	}

	// Flush series list.
	nn, err := sw.WriteTo(w)
	*n += nn
	if err != nil {
		return err
	}

	// Add series to each measurement and key/value.
	for i := range f.series {
		serie := &f.series[i]

		// Lookup series offset.
		serie.offset = sw.Offset(serie.name, serie.tags)
		if serie.offset == 0 {
			panic("series not found")
		}

		// Add series id to measurement, tag key, and tag value.
		mm := f.mms[string(serie.name)]
		mm.seriesIDs = append(mm.seriesIDs, serie.offset)
		f.mms[string(serie.name)] = mm

		// Add series id to each tag value.
		for _, tag := range serie.tags {
			t := mm.tagSet[string(tag.Key)]

			v := t.tagValues[string(tag.Value)]
			v.seriesIDs = append(v.seriesIDs, serie.offset)
			t.tagValues[string(tag.Value)] = v
		}
	}

	return nil
}

func (f *LogFile) writeTagsetsTo(w io.Writer, names []string, n *int64) error {
	for _, name := range names {
		if err := f.writeTagsetTo(w, name, n); err != nil {
			return err
		}
	}
	return nil
}

// writeTagsetTo writes a single tagset to w and saves the tagset offset.
func (f *LogFile) writeTagsetTo(w io.Writer, name string, n *int64) error {
	mm := f.mms[name]

	tw := NewTagBlockWriter()
	for _, tag := range mm.tagSet {
		// Mark tag deleted.
		if tag.deleted {
			tw.DeleteTag(tag.name)
			continue
		}

		// Add each value.
		for _, value := range tag.tagValues {
			sort.Sort(uint32Slice(value.seriesIDs))
			tw.AddTagValue(tag.name, value.name, value.deleted, value.seriesIDs)
		}
	}

	// Save tagset offset to measurement.
	mm.offset = *n

	// Write tagset to writer.
	nn, err := tw.WriteTo(w)
	*n += nn
	if err != nil {
		return err
	}

	// Save tagset offset to measurement.
	mm.size = *n - mm.offset

	f.mms[name] = mm

	return nil
}

func (f *LogFile) writeMeasurementBlockTo(w io.Writer, names []string, n *int64) error {
	mw := NewMeasurementBlockWriter()

	// Add measurement data.
	for _, mm := range f.mms {
		mw.Add(mm.name, mm.offset, mm.size, mm.seriesIDs)
	}

	// Write data to writer.
	nn, err := mw.WriteTo(w)
	*n += nn
	if err != nil {
		return err
	}

	return nil
}

// reset clears all the compaction fields on the in-memory index.
func (f *LogFile) reset() {
	// Clear series compaction fields.
	for i := range f.series {
		f.series[i].offset = 0
	}

	// Clear measurement compaction fields.
	for name, mm := range f.mms {
		mm.offset, mm.size, mm.seriesIDs = 0, 0, nil
		for key, tagSet := range mm.tagSet {
			for value, tagValue := range tagSet.tagValues {
				tagValue.seriesIDs = nil
				tagSet.tagValues[value] = tagValue
			}
			mm.tagSet[key] = tagSet
		}
		f.mms[name] = mm
	}
}

// LogEntry represents a single log entry in the write-ahead log.
type LogEntry struct {
	Flag     byte        // flag
	Name     []byte      // measurement name
	Tags     models.Tags // tagset
	Checksum uint32      // checksum of flag/name/tags.
	Size     int         // total size of record, in bytes.
}

// UnmarshalBinary unmarshals data into e.
func (e *LogEntry) UnmarshalBinary(data []byte) error {
	start := len(data)

	// Parse flag data.
	e.Flag, data = data[0], data[1:]

	// Parse name.
	sz, n := binary.Uvarint(data)
	e.Name, data = data[n:n+int(sz)], data[n+int(sz):]

	// Parse tag count.
	tagN, n := binary.Uvarint(data)
	data = data[n:]

	// Parse tags.
	tags := make(models.Tags, tagN)
	for i := range tags {
		tag := &tags[i]

		// Parse key.
		sz, n := binary.Uvarint(data)
		tag.Key, data = data[n:n+int(sz)], data[n+int(sz):]

		// Parse value.
		sz, n = binary.Uvarint(data)
		tag.Value, data = data[n:n+int(sz)], data[n+int(sz):]
	}

	// Parse checksum.
	e.Checksum, data = binary.BigEndian.Uint32(data[:4]), data[4:]

	// Save length of elem.
	e.Size = start - len(data)

	return nil
}

// appendLogEntry appends to dst and returns the new buffer.
// This updates the checksum on the entry.
func appendLogEntry(dst []byte, e *LogEntry) []byte {
	var buf [binary.MaxVarintLen64]byte
	start := len(dst)

	// Append flag.
	dst = append(dst, e.Flag)

	// Append name.
	n := binary.PutUvarint(buf[:], uint64(len(e.Name)))
	dst = append(dst, buf[:n]...)
	dst = append(dst, e.Name...)

	// Append tag count.
	n = binary.PutUvarint(buf[:], uint64(len(e.Tags)))
	dst = append(dst, buf[:n]...)

	// Append key/value pairs.
	for i := range e.Tags {
		t := &e.Tags[i]

		// Append key.
		n := binary.PutUvarint(buf[:], uint64(len(t.Key)))
		dst = append(dst, buf[:n]...)
		dst = append(dst, t.Key...)

		// Append value.
		n = binary.PutUvarint(buf[:], uint64(len(t.Value)))
		dst = append(dst, buf[:n]...)
		dst = append(dst, t.Value...)
	}

	// Calculate checksum.
	e.Checksum = crc32.ChecksumIEEE(dst[start:])

	// Append checksum.
	binary.BigEndian.PutUint32(buf[:4], e.Checksum)
	dst = append(dst, buf[:4]...)

	return dst
}

type logSerie struct {
	name    []byte
	tags    models.Tags
	deleted bool
	offset  uint32
}

func (s *logSerie) Name() []byte        { return s.name }
func (s *logSerie) Tags() models.Tags   { return s.tags }
func (s *logSerie) Deleted() bool       { return s.deleted }
func (s *logSerie) Expr() influxql.Expr { return nil }

type logSeries []logSerie

func (a logSeries) Len() int      { return len(a) }
func (a logSeries) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a logSeries) Less(i, j int) bool {
	if cmp := bytes.Compare(a[i].name, a[j].name); cmp != 0 {
		return cmp == -1
	}
	return models.CompareTags(a[i].tags, a[j].tags) == -1
}

// insert adds or updates a series in the list.
func (a *logSeries) insert(name []byte, tags models.Tags, deleted bool) {
	i := sort.Search(len(*a), func(i int) bool {
		if cmp := bytes.Compare((*a)[i].name, name); cmp != 0 {
			return cmp != -1
		}
		return models.CompareTags((*a)[i].tags, tags) != -1
	})

	// Update entry if it already exists.
	if i < len(*a) && bytes.Equal((*a)[i].name, name) && (*a)[i].tags.Equal(tags) {
		(*a)[i].deleted = deleted
		return
	}

	// Otherwise insert new entry.
	(*a) = append(*a, logSerie{})
	copy((*a)[i+1:], (*a)[i:])
	(*a)[i] = logSerie{name: name, tags: tags, deleted: deleted}
}

// logMeasurements represents a map of measurement names to measurements.
type logMeasurements map[string]logMeasurement

// names returns a sorted list of measurement names.
func (m logMeasurements) names() []string {
	a := make([]string, 0, len(m))
	for name := range m {
		a = append(a, name)
	}
	sort.Strings(a)
	return a
}

type logMeasurement struct {
	name    []byte
	tagSet  map[string]logTagSet
	deleted bool
	series  logSeries

	// Compaction fields.
	offset    int64    // tagset offset
	size      int64    // tagset size
	seriesIDs []uint32 // series offsets
}

func (m *logMeasurement) Name() []byte                   { return m.name }
func (m *logMeasurement) Deleted() bool                  { return m.deleted }
func (m *logMeasurement) TagKeyIterator() TagKeyIterator { panic("TODO") }

// logMeasurementSlice is a sortable list of log measurements.
type logMeasurementSlice []logMeasurement

func (a logMeasurementSlice) Len() int           { return len(a) }
func (a logMeasurementSlice) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a logMeasurementSlice) Less(i, j int) bool { return bytes.Compare(a[i].name, a[j].name) == -1 }

// logMeasurementIterator represents an iterator over a slice of measurements.
type logMeasurementIterator struct {
	mms []logMeasurement
}

// Next returns the next element in the iterator.
func (itr *logMeasurementIterator) Next() (e MeasurementElem) {
	if len(itr.mms) == 0 {
		return nil
	}
	e, itr.mms = &itr.mms[0], itr.mms[1:]
	return e
}

type logTagSet struct {
	name      []byte
	tagValues map[string]logTagValue
	deleted   bool
}

type logTagValue struct {
	name    []byte
	deleted bool
	entries []LogEntry

	// Compaction fields.
	seriesIDs []uint32
}

// insertEntry inserts an entry into the tag value in sorted order.
// If another entry matches the name/tags then it is overrwritten.
func (tv *logTagValue) insertEntry(e LogEntry) {
	i := sort.Search(len(tv.entries), func(i int) bool {
		if cmp := bytes.Compare(tv.entries[i].Name, e.Name); cmp != 0 {
			return cmp != -1
		}
		return models.CompareTags(tv.entries[i].Tags, e.Tags) != -1
	})

	// Update entry if it already exists.
	if i < len(tv.entries) && bytes.Equal(tv.entries[i].Name, e.Name) && tv.entries[i].Tags.Equal(e.Tags) {
		tv.entries[i] = e
		return
	}

	// Otherwise insert new entry.
	tv.entries = append(tv.entries, LogEntry{})
	copy(tv.entries[i+1:], tv.entries[i:])
	tv.entries[i] = e
}

// logSeriesIterator represents an iterator over a slice of series.
type logSeriesIterator struct {
	series logSeries
}

// Next returns the next element in the iterator.
func (itr *logSeriesIterator) Next() (e SeriesElem) {
	if len(itr.series) == 0 {
		return nil
	}
	e, itr.series = &itr.series[0], itr.series[1:]
	return e
}
