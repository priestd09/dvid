/*
	Package tarsupervoxels implements DVID support for data blobs associated with supervoxels.
*/
package tarsupervoxels

import (
	"archive/tar"
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/datatype/common/labels"
	"github.com/janelia-flyem/dvid/datatype/labelmap"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/server"
	"github.com/janelia-flyem/dvid/storage"
)

const (
	Version  = "0.1"
	RepoURL  = "github.com/janelia-flyem/dvid/datatype/tarsupervoxels"
	TypeName = "tarsupervoxels"
)

const helpMessage = `
API for 'keyvalue' datatype (github.com/janelia-flyem/dvid/datatype/tarsupervoxels)
=============================================================================

Command-line:

$ dvid repo <UUID> new tarsupervoxels <data name> <settings...>

	Adds newly named supervoxels tar support to repo with specified UUID.

	Example:

	$ dvid repo 3f8c new tarsupervoxels stuff

	Arguments:

	UUID           Hexidecimal string with enough characters to uniquely identify a version node.
	data name      Name of data to create, e.g., "supervoxel-meshes"
	settings       Configuration settings in "key=value" format separated by spaces.

	
	------------------

HTTP API (Level 2 REST):

Note that browsers support HTTP PUT and DELETE via javascript but only GET/POST are
included in HTML specs.  For ease of use in constructing clients, HTTP POST is used
to create or modify resources in an idempotent fashion.

GET  <api URL>/node/<UUID>/<data name>/help

	Returns data-specific help message.


GET  <api URL>/node/<UUID>/<data name>/info
POST <api URL>/node/<UUID>/<data name>/info

	Retrieves or puts data properties.

	Example: 

	GET <api URL>/node/3f8c/supervoxel-meshes/info

	Returns JSON with configuration settings.

	Arguments:

	UUID          Hexidecimal string with enough characters to uniquely identify a version node.
	data name     Name of tarsupervoxels data instance.


POST <api URL>/node/<UUID>/<data name>/sync?<options>

    Establishes labelmap for which supervoxel mapping is used.  Expects JSON to be POSTed
    with the following format:

    { "sync": "segmentation" }

	To delete syncs, pass an empty string of names with query string "replace=true":

	{ "sync": "" }

    The tarsupervoxels data type only accepts syncs to label instances that provide supervoxel info.

    GET Query-string Options:

    replace    Set to "true" if you want passed syncs to replace and not be appended to current syncs.
			   Default operation is false.

GET  <api URL>/node/<UUID>/<data name>/supervoxel/<id>
POST <api URL>/node/<UUID>/<data name>/supervoxel/<id>
DEL  <api URL>/node/<UUID>/<data name>/supervoxel/<id> 

	Performs get, put or delete of data on a supervoxel depending on the HTTP verb.  

	Example: 

	GET <api URL>/node/3f8c/supervoxel-meshes/supervoxel/18473948

		Returns the data associated with the supervoxel 18473948 of instance "supervoxel-meshes".

	POST <api URL>/node/3f8c/supervoxel-meshes/supervoxel/18473948

		Stores data associated with supervoxel 18473948 of instance 
		"supervoxel-meshes".

	The "Content-type" of the HTTP GET response and POST payload are "application/octet-stream" for arbitrary binary data.

	Arguments:

	UUID          Hexidecimal string with enough characters to uniquely identify a version node.
	data name     Name of tarsupervoxels data instance.
	label         The supervoxel id.

GET  <api URL>/node/<UUID>/<data name>/tarfile/<label> 

	Returns a tarfile of all supervoxel data that has been mapped to the given label.
	File names within the tarfile will be the supervoxel id without extension.  

	Example: 

	GET <api URL>/node/3f8c/supervoxel-meshes/tarfile/18473948

	The "Content-type" of the HTTP response is "application/tar".

	Arguments:

	UUID          Hexidecimal string with enough characters to uniquely identify a version node.
	data name     Name of tarsupervoxels data instance.
	label         The label (body) id.
	
POST <api URL>/node/<UUID>/<data name>/load

	Allows bulk-loading of tarfile with supervoxels data.  Each tarred file should
	have the supervoxel id as the filename *minus* the extension, e.g., 18491823.dat
	would be stored under supervoxel 18491823.

	Arguments:

	UUID          Hexidecimal string with enough characters to uniquely identify a version node.
	data name     Name of tarsupervoxels data instance.

`

func init() {
	datastore.Register(NewType())

	// Need to register types that will be used to fulfill interfaces.
	gob.Register(&Type{})
	gob.Register(&Data{})
}

// Type embeds the datastore's Type to create a unique type for keyvalue functions.
type Type struct {
	datastore.Type
}

// NewType returns a pointer to a new keyvalue Type with default values set.
func NewType() *Type {
	dtype := new(Type)
	dtype.Type = datastore.Type{
		Name:    TypeName,
		URL:     RepoURL,
		Version: Version,
		Requirements: &storage.Requirements{
			Batcher: true,
		},
	}
	return dtype
}

// --- TypeService interface ---

// NewDataService returns a pointer to new keyvalue data with default values.
func (dtype *Type) NewDataService(uuid dvid.UUID, id dvid.InstanceID, name dvid.InstanceName, c dvid.Config) (datastore.DataService, error) {
	basedata, err := datastore.NewDataService(dtype, uuid, id, name, c)
	if err != nil {
		return nil, err
	}
	extension, found, err := c.GetString("Extension")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("tarsupervoxels instances must have Extension set in the configuration")
	}
	return &Data{Data: basedata, Extension: extension}, nil
}

func (dtype *Type) Help() string {
	return fmt.Sprintf(helpMessage)
}

// GetByUUIDName returns a pointer to tarsupervoxels data given a UUID and data name.
func GetByUUIDName(uuid dvid.UUID, name dvid.InstanceName) (*Data, error) {
	source, err := datastore.GetDataByUUIDName(uuid, name)
	if err != nil {
		return nil, err
	}
	data, ok := source.(*Data)
	if !ok {
		return nil, fmt.Errorf("Instance '%s' is not a tarsupervoxels datatype!", name)
	}
	return data, nil
}

type mappedLabelType interface {
	GetSupervoxels(dvid.VersionID, uint64) (labels.Set, error)
	GetMappedLabels(dvid.VersionID, []uint64) ([]uint64, error)
	DataName() dvid.InstanceName
}

// Data embeds the datastore's Data and extends it with keyvalue properties (none for now).
type Data struct {
	*datastore.Data

	// Extension is the expected extension for blobs uploaded.
	// If no extension is given, it is "dat" by default.
	Extension string
}

func (d *Data) getSyncedLabels() mappedLabelType {
	for dataUUID := range d.SyncedData() {
		ldata, err := labelmap.GetByDataUUID(dataUUID)
		if err == nil {
			return ldata
		}
	}
	return nil
}

func (d *Data) Equals(d2 *Data) bool {
	if !d.Data.Equals(d2.Data) {
		return false
	}
	return true
}

type propsJSON struct {
	Extension string
}

func (d *Data) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Base     *datastore.Data
		Extended propsJSON
	}{
		d.Data,
		propsJSON{
			Extension: d.Extension,
		},
	})
}

func (d *Data) GobDecode(b []byte) error {
	buf := bytes.NewBuffer(b)
	dec := gob.NewDecoder(buf)
	if err := dec.Decode(&(d.Data)); err != nil {
		return err
	}
	if err := dec.Decode(&(d.Extension)); err != nil {
		return fmt.Errorf("decoding tarsupervoxels %q: no Extension", d.DataName())
	}
	return nil
}

func (d *Data) GobEncode() ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(d.Data); err != nil {
		return nil, err
	}
	if err := enc.Encode(d.Extension); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (d *Data) getRootContext(uuid dvid.UUID) (*datastore.VersionedCtx, error) {
	root, err := datastore.GetRepoRoot(uuid)
	if err != nil {
		return nil, err
	}
	v, err := datastore.VersionFromUUID(root)
	if err != nil {
		return nil, err
	}
	return datastore.NewVersionedCtx(d, v), nil
}

// GetData gets data for a supervoxel where the returned bool is true if data is found
func (d *Data) GetData(uuid dvid.UUID, supervoxel uint64) ([]byte, bool, error) {
	db, err := datastore.GetKeyValueDB(d)
	if err != nil {
		return nil, false, err
	}
	tk, err := NewTKey(supervoxel, d.Extension)
	if err != nil {
		return nil, false, err
	}
	ctx, err := d.getRootContext(uuid)
	if err != nil {
		return nil, false, err
	}
	data, err := db.Get(ctx, tk)
	if err != nil {
		return nil, false, fmt.Errorf("Error in retrieving supervoxel %d: %v", supervoxel, err)
	}
	if data == nil {
		return nil, false, nil
	}
	return data, true, nil
}

// PutData puts supervoxel data
func (d *Data) PutData(uuid dvid.UUID, supervoxel uint64, data []byte) error {
	db, err := datastore.GetKeyValueDB(d)
	if err != nil {
		return err
	}
	tk, err := NewTKey(supervoxel, d.Extension)
	if err != nil {
		return err
	}
	ctx, err := d.getRootContext(uuid)
	if err != nil {
		return err
	}
	return db.Put(ctx, tk, data)
}

// DeleteData deletes upervoxel data
func (d *Data) DeleteData(uuid dvid.UUID, supervoxel uint64) error {
	db, err := datastore.GetKeyValueDB(d)
	if err != nil {
		return err
	}
	tk, err := NewTKey(supervoxel, d.Extension)
	if err != nil {
		return err
	}
	ctx, err := d.getRootContext(uuid)
	if err != nil {
		return err
	}
	return db.Delete(ctx, tk)
}

// JSONString returns the JSON for this Data's configuration
func (d *Data) JSONString() (jsonStr string, err error) {
	m, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return string(m), nil
}

type fileData struct {
	header *tar.Header
	data   []byte
	err    error
}

func (d *Data) getSupervoxelGoroutine(db storage.KeyValueDB, ctx *datastore.VersionedCtx, supervoxels []uint64, outCh chan fileData, done <-chan struct{}) {
	dbt, canGetTimestamp := db.(storage.KeyValueTimestampGetter)
	for _, supervoxel := range supervoxels {
		tk, err := NewTKey(supervoxel, d.Extension)
		if err != nil {
			outCh <- fileData{err: err}
			continue
		}
		var modTime time.Time
		var data []byte
		if canGetTimestamp {
			data, modTime, err = dbt.GetWithTimestamp(ctx, tk)
		} else {
			data, err = db.Get(ctx, tk)
		}
		if err != nil {
			outCh <- fileData{err: err}
			continue
		}
		hdr := &tar.Header{
			Name:    fmt.Sprintf("%d.%s", supervoxel, d.Extension),
			Size:    int64(len(data)),
			Mode:    0755,
			ModTime: modTime,
		}
		select {
		case outCh <- fileData{header: hdr, data: data}:
		case <-done:
		}
	}
}

func (d *Data) sendTarfile(w http.ResponseWriter, uuid dvid.UUID, label uint64) error {
	db, err := datastore.GetKeyValueDB(d)
	if err != nil {
		return err
	}
	ldata := d.getSyncedLabels()
	if ldata == nil {
		return fmt.Errorf("data %q is not synced with any labelmap instance", d.DataName())
	}
	ctx, err := d.getRootContext(uuid)
	if err != nil {
		return err
	}
	v, err := datastore.VersionFromUUID(uuid)
	if err != nil {
		return err
	}
	supervoxels, err := ldata.GetSupervoxels(v, label)
	if err != nil {
		return err
	}
	if len(supervoxels) == 0 {
		return fmt.Errorf("label %d has no supervoxels", label)
	}
	numHandlers := 256 // Must be less than max open files, probably equal to multiple of disk queue
	svlist := make(map[int][]uint64, len(supervoxels))
	i := 0
	for supervoxel := range supervoxels {
		handler := i % numHandlers
		svs := svlist[handler]
		svs = append(svs, supervoxel)
		svlist[handler] = svs
	}

	done := make(chan struct{})
	defer close(done)
	outCh := make(chan fileData, len(supervoxels))
	for i := 0; i < numHandlers; i++ {
		go d.getSupervoxelGoroutine(db, ctx, svlist[i], outCh, done)
	}

	w.Header().Set("Content-type", "application/tar")
	tw := tar.NewWriter(w)
	defer tw.Close()
	for i := 0; i < len(supervoxels); i++ {
		fd := <-outCh
		if fd.err != nil {
			return err
		}
		if fd.header != nil {
			if err := tw.WriteHeader(fd.header); err != nil {
				return err
			}
			if _, err := tw.Write(fd.data); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Data) ingestTarfile(r *http.Request, uuid dvid.UUID) error {
	db, err := datastore.GetKeyValueDB(d)
	if err != nil {
		return err
	}
	ctx, err := d.getRootContext(uuid)
	if err != nil {
		return err
	}
	filenum := 1
	tr := tar.NewReader(r.Body)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		var supervoxel uint64
		var ext string
		n, err := fmt.Sscanf(hdr.Name, "%d.%s", &supervoxel, &ext)
		if err != nil || n != 2 {
			return fmt.Errorf("file %d name is invalid, expect supervoxel+ext: %s", filenum, hdr.Name)
		}
		if ext != d.Extension {
			return fmt.Errorf("file %d name has bad extension (expect %q): %s", filenum, d.Extension, hdr.Name)
		}
		if supervoxel == 0 {
			return fmt.Errorf("supervoxel 0 is reserved and cannot have data saved under 0 id")
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil {
			return err
		}
		tk, err := NewTKey(supervoxel, ext)
		if err := db.Put(ctx, tk, buf.Bytes()); err != nil {
			return err
		}
		filenum++
	}
	return nil
}

// --- DataService interface ---

func (d *Data) Help() string {
	return fmt.Sprintf(helpMessage)
}

// DoRPC acts as a switchboard for RPC commands.
func (d *Data) DoRPC(request datastore.Request, reply *datastore.Response) error {
	switch request.TypeCommand() {
	default:
		return fmt.Errorf("unknown command.  Data '%s' [%s] does not support '%s' command",
			d.DataName(), d.TypeName(), request.TypeCommand())
	}
}

// ServeHTTP handles all incoming HTTP requests for this data.
func (d *Data) ServeHTTP(uuid dvid.UUID, ctx *datastore.VersionedCtx, w http.ResponseWriter, r *http.Request) {
	timedLog := dvid.NewTimeLog()

	// Break URL request into arguments
	url := r.URL.Path[len(server.WebAPIPath):]
	parts := strings.Split(url, "/")
	if len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}

	if len(parts) < 4 {
		server.BadRequest(w, r, "incomplete API specification")
		return
	}

	var comment string
	action := strings.ToLower(r.Method)

	switch parts[3] {
	case "help":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, d.Help())
		return

	case "info":
		jsonStr, err := d.JSONString()
		if err != nil {
			server.BadRequest(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, jsonStr)
		return

	case "sync":
		if action != "post" {
			server.BadRequest(w, r, "Only POST allowed to sync endpoint")
			return
		}
		replace := r.URL.Query().Get("replace") == "true"
		if err := datastore.SetSyncByJSON(d, uuid, replace, r.Body); err != nil {
			server.BadRequest(w, r, err)
			return
		}

	case "load":
		if action != "post" {
			server.BadRequest(w, r, "only POST action is supported for the 'load' endpoint")
			return
		}
		if err := d.ingestTarfile(r, uuid); err != nil {
			server.BadRequest(w, r, err)
			return
		}
		comment = fmt.Sprintf("HTTP POST load on data %q", d.DataName())

	case "tarfile":
		if action != "get" {
			server.BadRequest(w, r, "only GET action is support for the 'tarfile' endpoint")
		}
		if len(parts) < 5 {
			server.BadRequest(w, r, "expect uint64 to follow 'tarfile' endpoint")
			return
		}
		label, err := strconv.ParseUint(parts[4], 10, 64)
		if err != nil {
			server.BadRequest(w, r, err)
			return
		}
		if label == 0 {
			server.BadRequest(w, r, "Label 0 is protected background value and cannot be used")
			return
		}
		if err := d.sendTarfile(w, uuid, label); err != nil {
			server.BadRequest(w, r, "can't send tarfile for label %d: %v", label, err)
			return
		}
		comment = fmt.Sprintf("HTTP GET tarfile on data %q, label %d", d.DataName(), label)

	case "supervoxel":
		if len(parts) < 5 {
			server.BadRequest(w, r, "expect uint64 to follow 'supervoxel' endpoint")
			return
		}
		supervoxel, err := strconv.ParseUint(parts[4], 10, 64)
		if err != nil {
			server.BadRequest(w, r, err)
			return
		}
		if supervoxel == 0 {
			server.BadRequest(w, r, "Supervoxel 0 is protected background value and cannot be used\n")
			return
		}

		switch action {
		case "get":
			data, found, err := d.GetData(uuid, supervoxel)
			if err != nil {
				server.BadRequest(w, r, err)
				return
			}
			if !found {
				http.Error(w, fmt.Sprintf("Supervoxel %d not found", supervoxel), http.StatusNotFound)
				return
			}
			if data != nil || len(data) > 0 {
				_, err = w.Write(data)
				if err != nil {
					server.BadRequest(w, r, err)
					return
				}
				w.Header().Set("Content-Type", "application/octet-stream")
			}
			comment = fmt.Sprintf("HTTP GET supervoxel %d of tarsupervoxels %q: %d bytes (%s)\n", supervoxel, d.DataName(), len(data), url)

		case "delete":
			if err := d.DeleteData(uuid, supervoxel); err != nil {
				server.BadRequest(w, r, err)
				return
			}
			comment = fmt.Sprintf("HTTP DELETE supervoxel %d data of tarsupervoxels %q (%s)\n", supervoxel, d.DataName(), url)

		case "post":
			data, err := ioutil.ReadAll(r.Body)
			if err != nil {
				server.BadRequest(w, r, err)
				return
			}
			if err := d.PutData(uuid, supervoxel, data); err != nil {
				server.BadRequest(w, r, err)
				return
			}
			comment = fmt.Sprintf("HTTP POST tarsupervoxels %q: %d bytes (%s)\n", d.DataName(), len(data), url)
		default:
			server.BadRequest(w, r, "supervoxel endpoint does not support %q HTTP verb", action)
			return
		}

	default:
		server.BadAPIRequest(w, r, d)
		return
	}

	timedLog.Infof(comment)
}
