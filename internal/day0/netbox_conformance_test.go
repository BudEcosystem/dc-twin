package day0

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
)

// NetBox's own import forms, not the twin's reading of them: testdata/netbox-import-<version>.json
// is what scripts/netbox-import-schema.py reads from a NetBox checkout at that release's tag
// (Meta.fields, the declared fields' required flags, choice sets and references). Every CSV the
// day-0 inputs write is checked against the form NetBox imports it with: each column is a field
// of the form, each field the form declares required is a column, each value of a choice field is
// one of its choice set's values, each field its model does not let be blank is a column (a
// ModelForm requires it whatever its default), and each reference to an object another of the CSVs makes names
// it by the attribute NetBox looks it up by (to_field_name), in a file imported before it.

// netboxRelease is the release of NetBoxVersion's series the forms were read from.
const netboxRelease = "4.2.9"

type netboxField struct {
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
	Choices  string `json:"choices"`
	Model    string `json:"model"`
	To       string `json:"to"`
}

type netboxSchema struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Forms   map[string]struct {
		Fields   []string               `json:"fields"`
		Declared map[string]netboxField `json:"declared"`
		// ModelRequired are the fields the form takes from its model without declaring them that
		// the model does not let be blank: Django's ModelForm requires them
		ModelRequired []string `json:"modelRequired"`
	} `json:"forms"`
	Choices map[string][]string `json:"choices"`
}

// netboxForms is the import form each file is imported with, and the model its rows make.
var netboxForms = map[string]struct{ form, model string }{
	"custom_fields.csv": {"CustomFieldImportForm", "CustomField"},
	"sites.csv":         {"SiteImportForm", "Site"},
	"racks.csv":         {"RackImportForm", "Rack"},
	"manufacturers.csv": {"ManufacturerImportForm", "Manufacturer"},
	"platforms.csv":     {"PlatformImportForm", "Platform"},
	"device_types.csv":  {"DeviceTypeImportForm", "DeviceType"},
	"device_roles.csv":  {"DeviceRoleImportForm", "DeviceRole"},
	"devices.csv":       {"DeviceImportForm", "Device"},
	"vrfs.csv":          {"VRFImportForm", "VRF"},
	"interfaces.csv":    {"InterfaceImportForm", "Interface"},
	"mac_addresses.csv": {"MACAddressImportForm", "MACAddress"},
	"cables.csv":        {"CableImportForm", "Cable"},
	"rirs.csv":          {"RIRImportForm", "RIR"},
	"asns.csv":          {"ASNImportForm", "ASN"},
	"vlans.csv":         {"VLANImportForm", "VLAN"},
	"prefixes.csv":      {"PrefixImportForm", "Prefix"},
	"ip_addresses.csv":  {"IPAddressImportForm", "IPAddress"},
}

// netboxObjectType is the object type (app_label.model) of the models the CSVs make custom
// fields for.
var netboxObjectType = map[string]string{"Device": "dcim.device", "Interface": "dcim.interface"}

func TestNetBoxImportForms(t *testing.T) {
	b, err := os.ReadFile("testdata/netbox-import-" + netboxRelease + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var nb netboxSchema
	if err := json.Unmarshal(b, &nb); err != nil {
		t.Fatal(err)
	}
	t.Logf("NetBox %s (%s)", nb.Version, nb.Commit)
	if !strings.HasPrefix(nb.Version, NetBoxVersion+".") {
		t.Fatalf("the forms are NetBox %s's, the CSVs are written for %s", nb.Version, NetBoxVersion)
	}
	d := fabric("given")
	// a segment on an estate network (oob), named and not a prefix, as the OOB fabric's are
	d.Switches[1].Vlans = append(d.Switches[1].Vlans, Vlan{Name: "Vlan1001", ID: 1001, Network: "oob", Gateway: "10.31.0.1/24"})
	out := netboxCSVs(d)
	// made[model][attribute] is every value a file imported so far gives that attribute
	made := map[string]map[string][]string{}
	for _, file := range netboxOrder {
		f, ok := netboxForms[file]
		if !ok {
			t.Fatalf("%s: no NetBox import form is known for it", file)
		}
		form, ok := nb.Forms[f.form]
		if !ok {
			t.Fatalf("%s: NetBox %s has no %s", file, nb.Version, f.form)
		}
		rows, err := csv.NewReader(bytes.NewReader(out[file])).ReadAll()
		if err != nil || len(rows) == 0 {
			t.Fatalf("%s: %v (%d rows)", file, err, len(rows))
		}
		head := rows[0]
		for _, col := range head {
			if cf, ok := strings.CutPrefix(col, "cf_"); ok {
				// a custom field's column (NetBox's cf_<name>): one custom_fields.csv makes, for
				// this file's object type
				fields := made["CustomField"]
				i := slices.Index(fields["name"], cf)
				if i < 0 || !slices.Contains(strings.Split(fields["object_types"][i], ","), netboxObjectType[f.model]) {
					t.Errorf("%s: column %q is no custom field custom_fields.csv makes for %s", file, col, netboxObjectType[f.model])
				}
				continue
			}
			if !slices.Contains(form.Fields, col) && col != "id" {
				t.Errorf("%s: column %q is not a field of NetBox's %s (%v)", file, col, f.form, form.Fields)
			}
		}
		for name, fd := range form.Declared {
			if fd.Required && slices.Contains(form.Fields, name) && !slices.Contains(head, name) {
				t.Errorf("%s: NetBox's %s requires %q", file, f.form, name)
			}
		}
		for _, name := range form.ModelRequired {
			if !slices.Contains(head, name) {
				t.Errorf("%s: NetBox's %s requires %q (its model's field, not blank)", file, f.form, name)
			}
		}
		for _, row := range rows[1:] {
			for i, col := range head {
				v := row[i]
				// an address or prefix column carries its mask, as NetBox's IP fields require
				if (col == "prefix" || col == "address") && v != "" {
					if _, err := netip.ParsePrefix(v); err != nil {
						t.Errorf("%s: %s %q is not an address with its mask: %v", file, col, v, err)
					}
				}
				fd, ok := form.Declared[col]
				if !ok || v == "" {
					continue
				}
				if fd.Choices != "" {
					if vals, ok := nb.Choices[fd.Choices]; ok && !slices.Contains(vals, v) {
						t.Errorf("%s: %s %q is not one of NetBox's %s", file, col, v, fd.Choices)
					}
				}
				if fd.Model == "" || fd.To == "" {
					continue
				}
				known, ok := made[fd.Model]
				if !ok {
					continue // an object the CSVs do not make (a tenant, a tag)
				}
				vals := []string{v}
				if strings.HasSuffix(fd.Kind, "MultipleChoiceField") {
					vals = strings.Split(v, ",")
				}
				for _, v := range vals {
					if !slices.Contains(known[fd.To], v) {
						t.Errorf("%s: %s %q names no %s by its %s among the files imported before (%v)", file, col, v, fd.Model, fd.To, known[fd.To])
					}
				}
			}
		}
		m := map[string][]string{}
		for _, row := range rows[1:] {
			for i, col := range head {
				m[col] = append(m[col], row[i])
			}
		}
		made[f.model] = m
	}
	t.Logf("%d files checked against NetBox %s's import forms", len(netboxOrder), nb.Version)
}
