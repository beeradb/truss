package inventory

import (
	"strings"
	"testing"
)

// validHostJSON, etc. are minimal-but-complete records: every field the
// schema declares, so DisallowUnknownFields has nothing to trip over
// before the test adds the one thing it means to test.
const validHostJSON = `{
	"schema": "truss.host/v1",
	"name": "alpha",
	"kind": "vm",
	"role": "k8s-node",
	"provisioned_by": "hosts/alpha",
	"config": "ansible/alpha",
	"tailnet_tags": ["k8s"],
	"cluster": "prod",
	"frozen": false,
	"decommissioned": false
}`

const validClusterJSON = `{
	"schema": "truss.cluster/v1",
	"name": "prod",
	"distribution": "k3s",
	"hosts": ["alpha"],
	"baseline": "baselines/prod",
	"capabilities": ["ingress"],
	"kubeconfig_item": "prod-kubeconfig",
	"has_ha_vault": true
}`

const validProjectJSON = `{
	"schema": "truss.project/v1",
	"name": "wren",
	"environments": ["prod"]
}`

const validEnvironmentJSON = `{
	"schema": "truss.environment/v1",
	"project": "wren",
	"name": "prod",
	"shape": "kubernetes",
	"placement": {"cluster": "prod", "namespace": "wren-prod", "host": null},
	"requires": ["ingress"],
	"vault": {"mount": "secret", "prefix": "wren/prod"},
	"frozen": false
}`

func TestDecodeRefusesAnUnknownField(t *testing.T) {
	cases := []struct {
		name   string
		valid  string
		decode func(string, []byte) error
	}{
		{"host", validHostJSON, func(stem string, data []byte) error { _, err := DecodeHost(stem, data); return err }},
		{"cluster", validClusterJSON, func(stem string, data []byte) error { _, err := DecodeCluster(stem, data); return err }},
		{"project", validProjectJSON, func(stem string, data []byte) error { _, err := DecodeProject(stem, data); return err }},
		{"environment", validEnvironmentJSON, func(stem string, data []byte) error { _, err := DecodeEnvironment(stem, data); return err }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Valid JSON must decode cleanly first, so a failure below is
			// known to come from the injected field and not from some
			// other mismatch between the fixture and the schema.
			if err := c.decode("stem", []byte(c.valid)); err != nil {
				t.Fatalf("valid fixture was refused: %v", err)
			}

			withExtra := strings.Replace(c.valid, "{", `{"unexpected_field": "boo",`, 1)
			err := c.decode("stem", []byte(withExtra))
			if err == nil {
				t.Fatalf("an unknown field was accepted: a newer writer's added field must be a refusal, not a silently dropped value")
			}
			if !strings.Contains(err.Error(), "stem") {
				t.Errorf("error %q does not name the stem", err.Error())
			}
		})
	}
}

func TestDecodeRefusesAMissingSchema(t *testing.T) {
	cases := []struct {
		name     string
		noSchema string
		decode   func(string, []byte) error
	}{
		{"host", `{"name":"alpha","kind":"vm","role":"r","provisioned_by":"p","config":null,"tailnet_tags":[],"cluster":null,"frozen":false,"decommissioned":false}`,
			func(stem string, data []byte) error { _, err := DecodeHost(stem, data); return err }},
		{"cluster", `{"name":"prod","distribution":"k3s","hosts":[],"baseline":"baselines/prod","capabilities":[],"kubeconfig_item":"item","has_ha_vault":true}`,
			func(stem string, data []byte) error { _, err := DecodeCluster(stem, data); return err }},
		{"project", `{"name":"wren","environments":[]}`,
			func(stem string, data []byte) error { _, err := DecodeProject(stem, data); return err }},
		{"environment", `{"project":"wren","name":"prod","shape":"kubernetes","placement":{"cluster":"prod","namespace":"wren-prod","host":null},"requires":[],"vault":{"mount":"secret","prefix":"wren/prod"},"frozen":false}`,
			func(stem string, data []byte) error { _, err := DecodeEnvironment(stem, data); return err }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.decode("stem", []byte(c.noSchema))
			if err == nil {
				t.Fatalf("a record with no schema field was accepted")
			}
			if !strings.Contains(err.Error(), "stem") {
				t.Errorf("error %q does not name the stem", err.Error())
			}
		})
	}
}
