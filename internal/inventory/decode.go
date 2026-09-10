package inventory

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// decode is the one parsing path every Decode* function below shares:
// refuse unknown fields, and refuse a record with no schema at all.
//
// DisallowUnknownFields matters because a newer writer's added field must
// be a refusal here, not a value this build silently drops on the floor --
// internal/parity/corpus.go:260 makes the identical choice, for the
// identical reason: a corpus (there) or an inventory record (here) that
// looks fully read but was not is worse than one that visibly failed to
// parse.
//
// A missing schema is refused at decode time rather than left for Check,
// because an empty string is not one of the values Check's "unrecognised
// schema" message is for -- it is not a record that named a version this
// build has never heard of, it is a record that never said which shape it
// is at all, and that has to stop before the bytes are even asserted to be
// a Host, a Cluster, a Project or an Environment.
func decode(stem string, kind string, data []byte, v interface{ schemaOf() string }) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("inventory: %s %q: %w", kind, stem, err)
	}
	if v.schemaOf() == "" {
		return fmt.Errorf("inventory: %s %q: no schema field -- every record must say which schema version it is", kind, stem)
	}
	return nil
}

func (h *Host) schemaOf() string        { return h.Schema }
func (c *Cluster) schemaOf() string     { return c.Schema }
func (p *Project) schemaOf() string     { return p.Schema }
func (e *Environment) schemaOf() string { return e.Schema }

// DecodeHost decodes one host record, refusing unknown fields and a
// missing schema. It does not check the schema is *recognised* -- that is
// Check's job, run over the whole snapshot -- only that decoding itself
// did not silently accept something this build cannot fully represent.
func DecodeHost(stem string, data []byte) (Host, error) {
	var h Host
	err := decode(stem, "host", data, &h)
	return h, err
}

// DecodeCluster decodes one cluster record. See DecodeHost.
func DecodeCluster(stem string, data []byte) (Cluster, error) {
	var c Cluster
	err := decode(stem, "cluster", data, &c)
	return c, err
}

// DecodeProject decodes one project record. See DecodeHost.
func DecodeProject(stem string, data []byte) (Project, error) {
	var p Project
	err := decode(stem, "project", data, &p)
	return p, err
}

// DecodeEnvironment decodes one environment record. See DecodeHost.
func DecodeEnvironment(stem string, data []byte) (Environment, error) {
	var e Environment
	err := decode(stem, "environment", data, &e)
	return e, err
}
