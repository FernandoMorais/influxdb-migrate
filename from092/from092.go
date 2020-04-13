package from092

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"path/filepath"
	"strings"
	"time"
	"errors"

	"github.com/boltdb/bolt"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/raft"
	"github.com/FernandoMorais/influxdb/client"
	"github.com/FernandoMorais/influxdb/influxql"
	"github.com/FernandoMorais/influxdb-migrate/database"
)

type replaceescaped struct {
	newtoken string
	replaced string
}

var (
	escapes = map[string]replaceescaped{
		`\,`: replaceescaped{newtoken: `§_§a§`, replaced: `,`},
		`\"`: replaceescaped{newtoken: `§_§b§`, replaced: `"`},
		`\ `: replaceescaped{newtoken: `§_§c§`, replaced: ` `},
		`\=`: replaceescaped{newtoken: `§_§d§`, replaced: `=`},
	}
)

type measurement struct {
	Name   string
	Fields []field
}

type field struct {
	ID   uint8             `json:"id,omitempty"`
	Name string            `json:"name,omitempty"`
	Type influxql.DataType `json:"type,omitempty"`
}

func GetPoints(datapath string,
	cdatabases chan<- database.Database,
	cpoints chan<- client.BatchPoints) {

	metapath := filepath.Join(datapath, "meta/raft.db")

	meta, err := bolt.Open(
		metapath,
		0600,
		&bolt.Options{Timeout: 1 * time.Second, ReadOnly: true})

	if err != nil {
		log.Fatalf("Error opening raft database from %s: %v\n", metapath, err)
	}

	var databases []database.Database
	err = meta.View(func(tx *bolt.Tx) error {
		logs := tx.Bucket([]byte("logs"))
		if logs == nil {
			log.Fatalf("Error opening logs bucket: %v\n", err)
		}
		err = logs.ForEach(func(k, v []byte) error {
			l := new(raft.Log)
			decodeMsgPack(v, l)
			databases = applycommand(databases, l.Data)
			return nil
		})
		if err != nil {
			log.Fatalf("Error traversing raft logs: %v\n", err)
		}

		return nil
	})

	if err != nil {
		log.Fatalf("Error reading raft database: %v\n", err)
	} else {
		err = meta.Close()
		if err != nil {
			log.Fatalf("Error closing raft database: %v\n", err)
		}
	}

	for _, db := range databases {
		cdatabases <- db
	}
	close(cdatabases)

	for _, db := range databases {
		for _, rp := range db.Policies {
			shardspath := filepath.Join(datapath, "data", db.Name, rp.Name)
			shards, err := ioutil.ReadDir(shardspath)
			if err != nil {
				continue
			}
			for _, sf := range shards {
				shdb, err := bolt.Open(
					filepath.Join(shardspath, sf.Name()),
					0600,
					&bolt.Options{Timeout: 1 * time.Second, ReadOnly: true})
				if err != nil {
					log.Fatalf("Error opening shard %s from rp %s on database %s: %v\n",
						sf.Name(), rp.Name, db.Name, err)
				}
				err = shdb.View(func(tx *bolt.Tx) error {
					mb := tx.Bucket([]byte("meta"))
					// defaults to b1 engine
					engine := []byte("b1")
					if mb != nil {
						if v := mb.Get([]byte("format")); v != nil {
							engine = v
						}
					}

					switch string(engine) {
					case "b1":
						return getb1points(tx, db.Name, rp.Name, sf.Name(), cpoints)
					case "bz1":
						return getbz1points(tx, db.Name, rp.Name, sf.Name(), cpoints)
					default:
						log.Fatalf("Unkown engine format %s for shard %s\n", engine, sf.Name())
					}
					return nil
				})

				if err != nil {
					log.Fatalf("Error traversing shard %s from rp %s on database %s: %v\n",
						sf.Name(), rp.Name, db.Name, err)
				}
			}
		}
	}
	close(cpoints)
}

func btou64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }

func u64tob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func getfields(mname string, m *measurementFields, b []byte) (ret map[string]interface{}, err error) {
	ret = make(map[string]interface{})
	for {
		if len(b) < 1 {
			break
		}
		fid := b[0]
		var f *field
		for _, mf := range m.Fields {
			if mf.ID == fid {
				f = mf
				break
			}
		}

		if f == nil {
			break
		}
		
		if f.ID == 0 {
			log.Fatalf("Couldn't find field %d in measurement %s\n", fid, mname)
		}
		var value interface{}
		switch f.Type {
		case influxql.Float:
			value = math.Float64frombits(binary.BigEndian.Uint64(b[1:9]))
			b = b[9:]
		case influxql.Integer:
			value = int64(binary.BigEndian.Uint64(b[1:9]))
			b = b[9:]
		case influxql.Boolean:
			if b[1] == 1 {
				value = true
			} else {
				value = false
			}
			b = b[2:]
		case influxql.String:
			size := binary.BigEndian.Uint16(b[1:3])
			b_size := len(b)

			defer func() {
				if r := recover(); r != nil {
					t_value := string(b[3:])

					fmt.Printf("\nERROR: --------------------------------------------------")
					fmt.Printf("\nERROR: converting field")
					fmt.Printf("\nERROR: --------------------------------------------------")
					fmt.Printf("\nERROR:  err: %+v", r)
					fmt.Printf("\nERROR:  f: %+v",f)
					fmt.Printf("\nERROR:  size: %v", size)
					fmt.Printf("\nERROR:  b_size: %v", b_size)
					fmt.Printf("\nERROR:  t_value: %s", t_value)
					fmt.Printf("\nERROR:  m: %+v", m)
					fmt.Printf("\nERROR:  ret: %+v", ret)
					fmt.Printf("\nERROR:  b: %v", b)
					fmt.Printf("\nERROR: --------------------------------------------------\n")

					// log.Fatalf("\nError converting field: %v\n\n", r)

					switch x := r.(type) {
					case string:
						err = errors.New(x)
					case error:
						err = x
					default:
						// Fallback err (per specs, error strings should be lowercase w/o punctuation
						err = errors.New("unknown panic")
					}
				}
			}() 
			
			value = string(b[3 : size+3])

			b = b[size+3:]
		default:
			log.Fatalf("unsupported value type during decode fields: %s", f.Type)
		}
		ret[f.Name] = value
	}
	return
}

func decodeMsgPack(buf []byte, out interface{}) error {
	r := bytes.NewBuffer(buf)
	hd := codec.MsgpackHandle{}
	dec := codec.NewDecoder(r, &hd)
	return dec.Decode(out)
}

func applycommand(dbs []database.Database, b []byte) []database.Database {
	var cmd Command
	if err := proto.Unmarshal(b, &cmd); err != nil {
		return dbs
	}
	updateddbs := dbs
	switch cmd.GetType() {
	case Command_CreateDatabaseCommand:
		ext, _ := proto.GetExtension(&cmd, E_CreateDatabaseCommand_Command)
		v := ext.(*CreateDatabaseCommand)
		if strings.HasSuffix(v.GetName(), "internal") {
			break
		}
		updateddbs = append(updateddbs, database.Database{Name: v.GetName()})
	case Command_DropDatabaseCommand:
		ext, _ := proto.GetExtension(&cmd, E_DropDatabaseCommand_Command)
		v := ext.(*DropDatabaseCommand)
		if len(dbs) > 0 {
			updateddbs = make([]database.Database, len(dbs)-1)
			for _, db := range dbs {
				if db.Name != v.GetName() {
					updateddbs = append(updateddbs, database.Database{Name: db.Name})
				}
			}
		}
	case Command_CreateRetentionPolicyCommand:
		ext, _ := proto.GetExtension(&cmd, E_CreateRetentionPolicyCommand_Command)
		v := ext.(*CreateRetentionPolicyCommand)
		for i, db := range updateddbs {
			if db.Name == v.GetDatabase() {
				rp := v.GetRetentionPolicy()
				if strings.HasSuffix(rp.GetName(), "internal") {
					continue
				}
				updateddbs[i].Policies = append(updateddbs[i].Policies,
					database.RetentionPolicy{
						Name:     rp.GetName(),
						Duration: time.Duration(rp.GetDuration()),
						ReplicaN: rp.GetReplicaN(),
					})
				break
			}
		}
	case Command_DropRetentionPolicyCommand:
		ext, _ := proto.GetExtension(&cmd, E_DropRetentionPolicyCommand_Command)
		v := ext.(*DropRetentionPolicyCommand)
		for i, db := range updateddbs {
			if db.Name == v.GetDatabase() {
				if len(updateddbs[i].Policies) > 0 {
					updateddbs[i].Policies = make([]database.RetentionPolicy, len(updateddbs[i].Policies)-1)
					for _, rp := range updateddbs[i].Policies {
						if rp.Name != v.GetName() {
							updateddbs[i].Policies = append(updateddbs[i].Policies,
								database.RetentionPolicy{
									Name:     rp.Name,
									Duration: rp.Duration,
									ReplicaN: rp.ReplicaN,
								})
						}
					}
				}
				break
			}
		}
	case Command_SetDefaultRetentionPolicyCommand:
		ext, _ := proto.GetExtension(&cmd, E_SetDefaultRetentionPolicyCommand_Command)
		v := ext.(*SetDefaultRetentionPolicyCommand)
		for i, db := range updateddbs {
			if db.Name == v.GetDatabase() {
				updateddbs[i].DefaultRetentionPolicy = v.GetName()
				break
			}
		}
	}
	return updateddbs
}

type measurementFields struct {
	Fields map[string]*field `json:"fields"`
}

// MarshalBinary encodes the object to a binary format.
func (m *measurementFields) MarshalBinary() ([]byte, error) {
	var pb MeasurementFields
	for _, f := range m.Fields {
		id := int32(f.ID)
		name := f.Name
		t := int32(f.Type)
		pb.Fields = append(pb.Fields, &Field{ID: &id, Name: &name, Type: &t})
	}
	return proto.Marshal(&pb)
}

// UnmarshalBinary decodes the object from a binary format.
func (m *measurementFields) UnmarshalBinary(buf []byte) error {
	var pb MeasurementFields
	if err := proto.Unmarshal(buf, &pb); err != nil {
		return err
	}
	m.Fields = make(map[string]*field)
	for _, f := range pb.Fields {
		m.Fields[f.GetName()] = &field{ID: uint8(f.GetID()), Name: f.GetName(), Type: influxql.DataType(f.GetType())}
	}
	return nil
}

func (m *measurementFields) String() string {
	b := &bytes.Buffer{}
	b.WriteString("[")
	for _, f := range m.Fields {
		b.WriteString(fmt.Sprintf("%d %s %v |", f.ID, f.Name, f.Type))
	}
	b.WriteString("]")
	return b.String()
}

func getb1points(tx *bolt.Tx,
	dbname, rpname, sfname string,
	cpoints chan<- client.BatchPoints) error {
	measurements := make(map[string]*measurementFields)
	fb := tx.Bucket([]byte("fields"))
	if fb == nil {
		log.Fatalf("Couldn't find bucket fields in shard %s\n", sfname)
	}
	if err := fb.ForEach(func(k, v []byte) error {
		mname := string(k)
		mf := &measurementFields{}
		err := mf.UnmarshalBinary(v)
		if err != nil {
			log.Fatalf("Error unmarshalling measurement %s: %v\n", mname, err)
		}
		measurements[mname] = mf
		return nil
	}); err != nil {
		return err
	}

	if err := tx.ForEach(func(name []byte, b *bolt.Bucket) error {
		bname := string(name)
		if bname != "fields" && bname != "series" && bname != "meta" && bname != "wal" {
			bnameescaped := bname
			for k, v := range escapes {
				bnameescaped = strings.Replace(bnameescaped, k, v.newtoken, -1)
			}
			bnamesplitted := strings.Split(bnameescaped, ",")
			mname := bnamesplitted[0]
			if _, ok := measurements[mname]; !ok {
				log.Printf("Couldn't find measurement %s in measurements\n", mname)
			} else {
				tags := make(map[string]string)
				for i := 1; i < len(bnamesplitted); i++ {
					ts := strings.Split(bnamesplitted[i], "=")
					tag := ts[1]
					for _, v := range escapes {
						tag = strings.Replace(tag, v.newtoken, v.replaced, -1)
					}
					tags[ts[0]] = tag
				}

				bp := client.BatchPoints{
					Database:        dbname,
					RetentionPolicy: rpname,
				}
				
				b.ForEach(func(k, v []byte) error {
					var fields map[string]interface {}
					var err error
					if fields, err = getfields(mname, measurements[mname], v); err != nil {
						log.Fatalf("\nError converting serie: %v\n\n", err)
					}

					bp.Points = append(bp.Points, client.Point{
						Measurement: mname,
						Time:        time.Unix(0, int64(btou64(k))),
						Tags:        tags,
						Fields:      fields,
					})
					return nil
				})
				cpoints <- bp
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func getbz1points(tx *bolt.Tx,
	dbname, rpname, sfname string,
	cpoints chan<- client.BatchPoints) error {

	fb := tx.Bucket([]byte("meta"))
	if fb == nil {
		log.Fatalf("Couldn't find bucket meta in shard %s\n", sfname)
	}
	v := fb.Get([]byte("fields"))

	data, err := snappy.Decode(nil, v)
	if err != nil {
		log.Fatalf("Error decoding fields bucket: %v\n", err)
	}

	measurements := make(map[string]*measurementFields)
	if err := json.Unmarshal(data, &measurements); err != nil {
		return fmt.Errorf("Error unmarshalling measurements: %v\n", err)
	}

	pb := tx.Bucket([]byte("points"))
	if pb == nil {
		log.Fatalf("Error retrieving points bucket from %s.%s.%s\n", dbname, rpname, sfname)
	}
	pb.ForEach(func(k, v []byte) error {
		bname := string(k)
		bnameescaped := bname
		for k, v := range escapes {
			bnameescaped = strings.Replace(bnameescaped, k, v.newtoken, -1)
		}
		bnamesplitted := strings.Split(bnameescaped, ",")
		mname := bnamesplitted[0]
		if _, ok := measurements[mname]; !ok {
			log.Fatalf("Couldn't find measurement %s in measurements\n", mname)
		} else {
			tags := make(map[string]string)
			for i := 1; i < len(bnamesplitted); i++ {
				ts := strings.Split(bnamesplitted[i], "=")
				tag := ts[1]
				for _, v := range escapes {
					tag = strings.Replace(tag, v.newtoken, v.replaced, -1)
				}
				tags[ts[0]] = tag
			}

			b := pb.Bucket(k)
			if b == nil {
				log.Fatalf("Error opening bucket %s\n", bname)
			} else {
				b.ForEach(func(k1, v1 []byte) error {
					buf, err := snappy.Decode(nil, v1[8:])
					if err != nil {
						log.Fatalf("Error decoding entry in %s.%s.%s.%s\n",
							dbname, rpname, sfname, bname)
					}
					var entries [][]byte
					for {
						if len(buf) == 0 {
							break
						}

						dataSize := entryDataSize(buf)
						entries = append(entries, buf[0:entryHeaderSize+dataSize])

						buf = buf[entryHeaderSize+dataSize:]
					}

					bp := client.BatchPoints{
						Database:        dbname,
						RetentionPolicy: rpname,
					}

					for _, b := range entries {
						var fields map[string]interface {}
						if fields, err = getfields(mname, measurements[mname], b[entryHeaderSize:]); err != nil {
							point := client.Point{
								Measurement: mname,
								Time:        time.Unix(0, int64(btou64(b[0:8]))),
								Tags:        tags,
								Fields:      fields,
							}

							fmt.Printf("\nERROR: --------------------------------------------------")
							fmt.Printf("\nERROR: Converting serie")
							fmt.Printf("\nERROR: --------------------------------------------------")
							fmt.Printf("\nERROR:  mname: %+v", mname)
							fmt.Printf("\nERROR:  time: %+v", time.Unix(0, int64(btou64(b[0:8]))))
							fmt.Printf("\nERROR:  tags: %+v", tags)
							fmt.Printf("\nERROR:  tags: Repository =~ /%s/ AND ImageName =~ /%s/ AND ID =~ /%s/ AND ImageVersion =~ /%s/ AND Package =~ /%s/", tags["Repository"], tags["ImageName"], tags["ID"], tags["ImageVersion"], tags["Package"])
							fmt.Printf("\nERROR:  fields: %+v", fields)
							fmt.Printf("\nERROR:  entryHeaderSize: %v", entryHeaderSize)
							fmt.Printf("\nERROR:  b: %v", b)
							fmt.Printf("\nERROR:  point: %+v", point)
							fmt.Printf("\nERROR: --------------------------------------------------\n")
	
							// log.Fatalf("\nError converting serie: %v\n\n", err)
						} else {
							bp.Points = append(bp.Points, client.Point{
								Measurement: mname,
								Time:        time.Unix(0, int64(btou64(b[0:8]))),
								Tags:        tags,
								Fields:      fields,
							})
						}
					}
					cpoints <- bp
					return nil
				})
			}
		}
		return nil
	})
	return nil
}

// entryHeaderSize is the number of bytes required for the header.
const entryHeaderSize = 8 + 4

// entryDataSize returns the size of an entry's data field, in bytes.
func entryDataSize(v []byte) int { return int(binary.BigEndian.Uint32(v[8:12])) }
