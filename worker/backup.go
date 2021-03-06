/*
 * Copyright (C) 2017 Dgraph Labs, Inc. and Contributors
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package worker

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"math/rand"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/badger/badger"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"github.com/dgraph-io/dgraph/group"
	"github.com/dgraph-io/dgraph/protos"
	"github.com/dgraph-io/dgraph/types"
	"github.com/dgraph-io/dgraph/types/facets"
	"github.com/dgraph-io/dgraph/x"
)

const numBackupRoutines = 10

type kv struct {
	prefix string
	list   *protos.PostingList
}

type skv struct {
	attr   string
	schema *protos.SchemaUpdate
}

// Map from our types to RDF type. Useful when writing storage types
// for RDF's in backup. This is the dgraph type name and rdf storage type
// might not be the same always (e.g. - datetime and bool).
var rdfTypeMap = map[types.TypeID]string{
	types.StringID:   "xs:string",
	types.DateTimeID: "xs:dateTime",
	types.IntID:      "xs:int",
	types.FloatID:    "xs:float",
	types.BoolID:     "xs:boolean",
	types.GeoID:      "geo:geojson",
	types.PasswordID: "pwd:password",
}

func toRDF(buf *bytes.Buffer, item kv) {
	pl := item.list
	for _, p := range pl.Postings {
		buf.WriteString(item.prefix)

		if !bytes.Equal(p.Value, nil) {
			// Value posting
			// Convert to appropriate type
			vID := types.TypeID(p.ValType)
			src := types.ValueForType(vID)
			src.Value = p.Value
			str, err := types.Convert(src, types.StringID)
			x.Check(err)
			buf.WriteByte('"')
			buf.WriteString(str.Value.(string))
			buf.WriteByte('"')
			if p.PostingType == protos.Posting_VALUE_LANG {
				buf.WriteByte('@')
				buf.WriteString(string(p.Metadata))
			} else if vID != types.BinaryID &&
				vID != types.DefaultID {
				rdfType, ok := rdfTypeMap[vID]
				x.AssertTruef(ok, "Didn't find RDF type for dgraph type: %+v", vID.Name())
				buf.WriteString("^^<")
				buf.WriteString(rdfType)
				buf.WriteByte('>')
			}
		} else {
			buf.WriteString("<0x")
			buf.WriteString(strconv.FormatUint(p.Uid, 16))
			buf.WriteByte('>')
		}
		// Label
		if len(p.Label) > 0 {
			buf.WriteString(" <")
			buf.WriteString(p.Label)
			buf.WriteByte('>')
		}
		// Facets.
		fcs := p.Facets
		if len(fcs) != 0 {
			buf.WriteString(" (")
			for i, f := range fcs {
				if i != 0 {
					buf.WriteByte(',')
				}
				buf.WriteString(f.Key)
				buf.WriteByte('=')
				fVal := &types.Val{Tid: types.StringID}
				x.Check(types.Marshal(facets.ValFor(f), fVal))
				if facets.TypeIDFor(f) == types.StringID {
					buf.WriteByte('"')
					buf.WriteString(fVal.Value.(string))
					buf.WriteByte('"')
				} else {
					buf.WriteString(fVal.Value.(string))
				}
			}
			buf.WriteByte(')')
		}
		// End dot.
		buf.WriteString(" .\n")
	}
}

func toSchema(buf *bytes.Buffer, s *skv) {
	if strings.ContainsRune(s.attr, ':') {
		buf.WriteRune('<')
		buf.WriteString(s.attr)
		buf.WriteRune('>')
	} else {
		buf.WriteString(s.attr)
	}
	buf.WriteByte(':')
	buf.WriteString(types.TypeID(s.schema.ValueType).Name())
	if s.schema.Directive == protos.SchemaUpdate_REVERSE {
		buf.WriteString(" @reverse")
	} else if s.schema.Directive == protos.SchemaUpdate_INDEX && len(s.schema.Tokenizer) > 0 {
		buf.WriteString(" @index(")
		buf.WriteString(strings.Join(s.schema.Tokenizer, ","))
		buf.WriteByte(')')
	}
	buf.WriteString(" . \n")
}

func writeToFile(fpath string, ch chan []byte) error {
	f, err := os.Create(fpath)
	if err != nil {
		return err
	}

	defer f.Close()
	x.Check(err)
	w := bufio.NewWriterSize(f, 1000000)
	gw, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}

	for buf := range ch {
		if _, err := gw.Write(buf); err != nil {
			return err
		}
	}
	if err := gw.Flush(); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	return w.Flush()
}

// Backup creates a backup of data by exporting it as an RDF gzip.
func backup(gid uint32, bdir string) error {
	// Use a goroutine to write to file.
	err := os.MkdirAll(bdir, 0700)
	if err != nil {
		return err
	}
	fpath := path.Join(bdir, fmt.Sprintf("dgraph-%d-%s.rdf.gz", gid,
		time.Now().Format("2006-01-02-15-04")))
	fspath := path.Join(bdir, fmt.Sprintf("dgraph-schema-%d-%s.rdf.gz", gid,
		time.Now().Format("2006-01-02-15-04")))
	fmt.Printf("Backing up at: %v, schema at %v\n", fpath, fspath)
	chb := make(chan []byte, 1000)
	errChan := make(chan error, 2)
	go func() {
		errChan <- writeToFile(fpath, chb)
	}()
	chsb := make(chan []byte, 1000)
	go func() {
		errChan <- writeToFile(fspath, chsb)
	}()

	// Use a bunch of goroutines to convert to RDF format.
	chkv := make(chan kv, 1000)
	var wg sync.WaitGroup
	wg.Add(numBackupRoutines)
	for i := 0; i < numBackupRoutines; i++ {
		go func(i int) {
			buf := new(bytes.Buffer)
			buf.Grow(50000)
			for item := range chkv {
				toRDF(buf, item)
				if buf.Len() >= 40000 {
					tmp := make([]byte, buf.Len())
					copy(tmp, buf.Bytes())
					chb <- tmp
					buf.Reset()
				}
			}
			if buf.Len() > 0 {
				tmp := make([]byte, buf.Len())
				copy(tmp, buf.Bytes())
				chb <- tmp
			}
			wg.Done()
		}(i)
	}

	// Use a goroutine to convert protos.Schema to string
	chs := make(chan *skv, 1000)
	wg.Add(1)
	go func() {
		buf := new(bytes.Buffer)
		buf.Grow(50000)
		for item := range chs {
			toSchema(buf, item)
			if buf.Len() >= 40000 {
				tmp := make([]byte, buf.Len())
				copy(tmp, buf.Bytes())
				chsb <- tmp
				buf.Reset()
			}
		}
		if buf.Len() > 0 {
			tmp := make([]byte, buf.Len())
			copy(tmp, buf.Bytes())
			chsb <- tmp
		}
		wg.Done()
	}()

	// Iterate over rocksdb.
	it := pstore.NewIterator(badger.DefaultIteratorOptions)
	defer it.Close()
	var lastPred string
	prefix := new(bytes.Buffer)
	prefix.Grow(100)
	var debugCount int
	for it.Rewind(); it.Valid(); debugCount++ {
		item := it.Item()
		key := item.Key()
		pk := x.Parse(key)

		if pk.IsIndex() {
			// Seek to the end of index keys.
			it.Seek(pk.SkipRangeOfSameType())
			continue
		}
		if pk.IsReverse() {
			// Seek to the end of reverse keys.
			it.Seek(pk.SkipRangeOfSameType())
			continue
		}
		if pk.Attr == "_uid_" || pk.Attr == "_predicate_" {
			// Skip the UID mappings.
			it.Seek(pk.SkipPredicate())
			continue
		}
		if pk.IsSchema() {
			if group.BelongsTo(pk.Attr) == gid {
				s := &protos.SchemaUpdate{}
				x.Check(s.Unmarshal(item.Value()))
				chs <- &skv{
					attr:   pk.Attr,
					schema: s,
				}
			}
			// skip predicate
			it.Next()
			continue
		}
		x.AssertTrue(pk.IsData())
		pred, uid := pk.Attr, pk.Uid
		if pred != lastPred && group.BelongsTo(pred) != gid {
			it.Seek(pk.SkipPredicate())
			continue
		}

		prefix.WriteString("<0x")
		prefix.WriteString(strconv.FormatUint(uid, 16))
		prefix.WriteString("> <")
		prefix.WriteString(pred)
		prefix.WriteString("> ")
		pl := &protos.PostingList{}
		x.Check(pl.Unmarshal(item.Value()))
		chkv <- kv{
			prefix: prefix.String(),
			list:   pl,
		}
		prefix.Reset()
		lastPred = pred
		it.Next()
	}

	close(chkv) // We have stopped output to chkv.
	close(chs)  // we have stopped output to chs (schema)
	wg.Wait()   // Wait for numBackupRoutines to finish.
	close(chb)  // We have stopped output to chb.
	close(chsb) // we have stopped output to chs (schema)

	err = <-errChan
	err = <-errChan
	return err
}

func handleBackupForGroup(ctx context.Context, reqId uint64, gid uint32) *protos.BackupPayload {
	n := groups().Node(gid)
	if n.AmLeader() {
		x.Trace(ctx, "Leader of group: %d. Running backup.", gid)
		if err := backup(gid, *backupPath); err != nil {
			x.TraceError(ctx, err)
			return &protos.BackupPayload{
				ReqId:  reqId,
				Status: protos.BackupPayload_FAILED,
			}
		}
		x.Trace(ctx, "Backup done for group: %d.", gid)
		return &protos.BackupPayload{
			ReqId:   reqId,
			Status:  protos.BackupPayload_SUCCESS,
			GroupId: gid,
		}
	}

	// I'm not the leader. Relay to someone who I think is.
	var addrs []string
	{
		// Try in order: leader of given group, any server from given group, leader of group zero.
		_, addr := groups().Leader(gid)
		addrs = append(addrs, addr)
		addrs = append(addrs, groups().AnyServer(gid))
		_, addr = groups().Leader(0)
		addrs = append(addrs, addr)
	}

	var conn *grpc.ClientConn
	for _, addr := range addrs {
		pl := pools().get(addr)
		var err error
		conn, err = pl.Get()
		if err == nil {
			x.Trace(ctx, "Relaying backup request for group %d to %q", gid, pl.Addr)
			defer pl.Put(conn)
			break
		}
		x.TraceError(ctx, err)
	}

	// Unable to find any connection to any of these servers. This should be exceedingly rare.
	// But probably not worthy of crashing the server. We can just skip the backup.
	if conn == nil {
		x.Trace(ctx, fmt.Sprintf("Unable to find a server to backup group: %d", gid))
		return &protos.BackupPayload{
			ReqId:   reqId,
			Status:  protos.BackupPayload_FAILED,
			GroupId: gid,
		}
	}

	c := protos.NewWorkerClient(conn)
	nr := &protos.BackupPayload{
		ReqId:   reqId,
		GroupId: gid,
	}
	nrep, err := c.Backup(ctx, nr)
	if err != nil {
		x.TraceError(ctx, err)
		return &protos.BackupPayload{
			ReqId:   reqId,
			Status:  protos.BackupPayload_FAILED,
			GroupId: gid,
		}
	}
	return nrep
}

// Backup request is used to trigger backups for the request list of groups.
// If a server receives request to backup a group that it doesn't handle, it would
// automatically relay that request to the server that it thinks should handle the request.
func (w *grpcWorker) Backup(ctx context.Context, req *protos.BackupPayload) (*protos.BackupPayload, error) {
	reply := &protos.BackupPayload{ReqId: req.ReqId}
	reply.Status = protos.BackupPayload_FAILED // Set by default.

	if ctx.Err() != nil {
		return reply, ctx.Err()
	}
	if !w.addIfNotPresent(req.ReqId) {
		reply.Status = protos.BackupPayload_DUPLICATE
		return reply, nil
	}

	chb := make(chan *protos.BackupPayload, 1)
	go func() {
		chb <- handleBackupForGroup(ctx, req.ReqId, req.GroupId)
	}()

	select {
	case rep := <-chb:
		return rep, nil
	case <-ctx.Done():
		return reply, ctx.Err()
	}
}

func BackupOverNetwork(ctx context.Context) error {
	// If we haven't even had a single membership update, don't run backup.
	if !HealthCheck() {
		x.Trace(ctx, "This server hasn't yet been fully initiated. Please retry later.")
		return x.Errorf("Uninitiated server. Please retry later")
	}
	// Let's first collect all groups.
	gids := groups().KnownGroups()

	for i, gid := range gids {
		if gid == 0 {
			gids[i] = gids[len(gids)-1]
			gids = gids[:len(gids)-1]
		}
	}

	ch := make(chan *protos.BackupPayload, len(gids))
	for _, gid := range gids {
		go func(group uint32) {
			reqId := uint64(rand.Int63())
			ch <- handleBackupForGroup(ctx, reqId, group)
		}(gid)
	}

	for i := 0; i < len(gids); i++ {
		bp := <-ch
		if bp.Status != protos.BackupPayload_SUCCESS {
			x.Trace(ctx, "Backup status: %v for group id: %d", bp.Status, bp.GroupId)
			return fmt.Errorf("Backup status: %v for group id: %d", bp.Status, bp.GroupId)
		}
		x.Trace(ctx, "Backup successful for group: %v", bp.GroupId)
	}
	x.Trace(ctx, "DONE backup")
	return nil
}
