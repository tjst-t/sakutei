// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package s3

import (
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
)

// decl は JSON を宣言の値（load と同じ正規形）にする。
func decl(t *testing.T, typ core.FieldType, s string) any {
	t.Helper()
	v, err := decodeJSON(s)
	if err != nil {
		t.Fatal(err)
	}
	n, err := core.Normalize(typ, v)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCanonPolicy_Equivalents(t *testing.T) {
	a := `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Principal":{"AWS":"*"},"Action":["s3:PutObject","s3:GetObject","s3:GetObject"],"Resource":"arn:aws:s3:::b/*","Condition":{"StringEquals":{"aws:SourceVpc":"vpc-1"}}}}`
	b := `{
	  "Statement": [{
	    "Resource": ["arn:aws:s3:::b/*"],
	    "Condition": {"StringEquals": {"aws:SourceVpc": ["vpc-1"]}},
	    "Action": ["s3:GetObject", "s3:PutObject"],
	    "Principal": {"AWS": ["*"]},
	    "Effect": "Allow"
	  }],
	  "Version": "2012-10-17"
	}`
	if !matchPolicy(decl(t, core.Map, a), decl(t, core.Map, b)) {
		ca, _ := CanonPolicy(a)
		cb, _ := CanonPolicy(b)
		t.Errorf("同じ意味のポリシーが違う:\n%s\n%s", ca, cb)
	}
	if !matchPolicy(decl(t, core.Map, `{"Statement":[{"Principal":"*"}]}`), decl(t, core.Map, `{"Statement":[{"Principal":{"AWS":["*"]}}]}`)) {
		t.Error(`Principal "*" と {"AWS":["*"]} が違う`)
	}
	if !matchPolicy(map[string]any{}, map[string]any{}) || matchPolicy(map[string]any{}, nil) {
		t.Error("空のポリシーの比べ方が誤り")
	}
	other := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":{"AWS":"*"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`
	if matchPolicy(decl(t, core.Map, a), decl(t, core.Map, other)) {
		t.Error("違うポリシーが同じ正規形")
	}
	if c, _ := CanonPolicy(`{"Statement":[{"Resource":"arn:aws:s3:::b/<x>&"}]}`); c != `{"Statement":[{"Resource":["arn:aws:s3:::b/<x>&"]}]}` {
		t.Errorf("HTML の文字をエスケープした: %s", c)
	}
	// 観測の値は正規形の不動点（core.Normalize で変わらない）。
	v, err := policyValue(a)
	if err != nil {
		t.Fatal(err)
	}
	if !matchPolicy(v, decl(t, core.Map, b)) {
		t.Errorf("観測の値 %v が宣言と合わない", v)
	}
}

func TestLifecycle_RoundTripAndEquivalents(t *testing.T) {
	d := decl(t, core.List, `[
	  {"ID": "logs", "Status": "Enabled", "Filter": {"And": {"Prefix": "logs/", "Tag": [{"Key": "k", "Value": "v"}, {"Key": "a", "Value": "b"}]}},
	   "Transition": [{"Days": 30, "StorageClass": "GLACIER"}, {"Days": 7, "StorageClass": "STANDARD_IA"}],
	   "Expiration": {"Days": 365}},
	  {"ID": "mpu", "Status": "Disabled", "Filter": {}, "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 3}},
	  {"ID": "date", "Status": "Enabled", "Expiration": {"Date": "2027-01-01T00:00:00Z"}}
	]`)
	rules, err := ParseLifecycle(d)
	if err != nil {
		t.Fatal(err)
	}
	// XML にして読み戻しても同じ正規形。
	back, err := lifecycleFromXML(lifecycleXML(rules))
	if err != nil {
		t.Fatal(err)
	}
	if !matchLifecycle(d, canonRules(back)) {
		t.Errorf("読み戻すと違う: %s", canonJSON(canonRules(back)))
	}
	// endpoint が返しうる別の形（ルール・タグの順、空の Filter・Prefix、日時の小数）。
	server := `<?xml version="1.0"?><LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	  <Rule><ID>date</ID><Filter><Prefix></Prefix></Filter><Status>Enabled</Status><Expiration><Date>2027-01-01T00:00:00.000Z</Date></Expiration></Rule>
	  <Rule><ID>mpu</ID><Status>Disabled</Status><AbortIncompleteMultipartUpload><DaysAfterInitiation>3</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule>
	  <Rule><ID>logs</ID><Filter><And><Tag><Key>a</Key><Value>b</Value></Tag><Prefix>logs/</Prefix><Tag><Key>k</Key><Value>v</Value></Tag></And></Filter><Status>Enabled</Status>
	    <Transition><Days>7</Days><StorageClass>STANDARD_IA</StorageClass></Transition><Transition><Days>30</Days><StorageClass>GLACIER</StorageClass></Transition>
	    <Expiration><Days>365</Days></Expiration></Rule>
	</LifecycleConfiguration>`
	sr, err := lifecycleFromXML([]byte(server))
	if err != nil {
		t.Fatal(err)
	}
	obs, err := core.Normalize(core.List, canonRules(sr))
	if err != nil {
		t.Fatal(err)
	}
	if !matchLifecycle(d, obs) {
		t.Errorf("endpoint の形が宣言と合わない: %s", canonJSON(obs))
	}
	if !matchLifecycle([]any{}, canonRules(nil)) {
		t.Error("空の lifecycle が合わない")
	}
	if _, err := ParseLifecycle(decl(t, core.List, `[{"ID":"a","Status":"Enabled","Bad Name":1}]`)); err == nil {
		t.Error("XML の名前にできないキーを受けた")
	}
	if _, err := ParseLifecycle(decl(t, core.List, `[{"Status":"Enabled"}]`)); err == nil {
		t.Error("ID の無いルールを受けた")
	}
}

func TestObjectLock_ParseAndCanon(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{}`, `{"enabled":true}`},
		{`{"enabled": false}`, `{"enabled":false}`},
		{`{"enable_on_create": true}`, `{"enabled":true}`},
		{`{"mode": "COMPLIANCE", "days": 30}`, `{"days":30,"enabled":true,"mode":"COMPLIANCE"}`},
		{`{"mode": "COMPLIANCE", "years": 7, "enable_on_create": false}`, `{"days":2555,"enabled":true,"mode":"COMPLIANCE"}`},
	} {
		l, err := ParseObjectLock(decl(t, core.Map, tc.in))
		if got := canonJSON(l.value()); err != nil || got != tc.want {
			t.Errorf("ParseObjectLock(%s) = %s, %v（want %s）", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{
		`{"mode": "COMPLIANCE"}`, `{"days": 3}`, `{"mode": "COMPLIANCE", "days": 3, "years": 1}`,
		`{"mode": "LEGAL", "days": 3}`, `{"mode": "COMPLIANCE", "days": 0}`, `{"mode": "COMPLIANCE", "days": -1}`,
		`{"mode": "COMPLIANCE", "days": 1.5}`, `{"mode": "COMPLIANCE", "days": "3"}`, `{"mode": "COMPLIANCE", "years": 101}`,
		`{"enable_on_create": "yes"}`, `{"Mode": "COMPLIANCE", "days": 3}`, `{"enabled": false, "mode": "GOVERNANCE", "days": 1}`,
	} {
		if _, err := ParseObjectLock(decl(t, core.Map, bad)); err == nil {
			t.Errorf("ParseObjectLock(%s) が断らない", bad)
		}
	}
	// years の宣言は Days の観測と合う。enable_on_create は比べない。
	if !matchLock(decl(t, core.Map, `{"enable_on_create": true, "mode": "COMPLIANCE", "years": 1}`), LockSpec{Enabled: true, Mode: "COMPLIANCE", Days: 365}.value()) {
		t.Error("years 1 と 365 日が合わない")
	}
	if matchLock(decl(t, core.Map, `{}`), nil) {
		t.Error("観測に無い object_lock が合った")
	}
}

func TestLockChange(t *testing.T) {
	gov1 := LockSpec{Enabled: true, Mode: ObjectLockGovernance, Days: 1}
	for _, tc := range []struct {
		from, to LockSpec
		want     string
	}{
		{gov1, LockSpec{Enabled: true, Mode: ObjectLockGovernance, Days: 2}, "1 → 2 日に延ばす"},
		{gov1, LockSpec{Enabled: true, Mode: ObjectLockGovernance, Years: 1}, "1 → 365 日に延ばす"},
		{LockSpec{Enabled: true, Mode: ObjectLockGovernance, Days: 30}, gov1, "30 → 1 日に縮める"},
		{gov1, LockSpec{Enabled: true}, "既定の保持（GOVERNANCE 1 日）を外す"},
		{gov1, LockSpec{Enabled: true, Mode: ObjectLockCompliance, Days: 1}, "GOVERNANCE → COMPLIANCE"},
		{LockSpec{}, LockSpec{Enabled: true}, "Object Lock を有効にする"},
	} {
		if got := lockChange(tc.from, tc.to); !strings.Contains(got, tc.want) {
			t.Errorf("lockChange(%+v → %+v) = %q（want %q を含む）", tc.from, tc.to, got, tc.want)
		}
	}
	if got := lockChange(gov1, gov1); got != "" {
		t.Errorf("変わらないのに %q", got)
	}
}
