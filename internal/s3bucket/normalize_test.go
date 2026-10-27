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

package s3bucket

import "testing"

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
	ca, err := CanonPolicy(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := CanonPolicy(b)
	if err != nil {
		t.Fatal(err)
	}
	if ca != cb {
		t.Errorf("equivalent policies differ:\n%s\n%s", ca, cb)
	}
	star, _ := CanonPolicy(`{"Statement":[{"Principal":"*"}]}`)
	aws, _ := CanonPolicy(`{"Statement":[{"Principal":{"AWS":["*"]}}]}`)
	if star != aws {
		t.Errorf(`Principal "*" (%s) and {"AWS":["*"]} (%s) differ`, star, aws)
	}
	if c, _ := CanonPolicy(" \n"); c != "" {
		t.Errorf("empty policy = %q", c)
	}
	// 中身が違えば違う（Effect・Statement の順）。
	other := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":{"AWS":"*"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`
	if co, _ := CanonPolicy(other); co == ca {
		t.Error("a different policy has the same canonical form")
	}
	if c, _ := CanonPolicy(`{"Statement":[{"Resource":"arn:aws:s3:::b/<x>&"}]}`); c != `{"Statement":[{"Resource":["arn:aws:s3:::b/<x>&"]}]}` {
		t.Errorf("HTML characters are escaped: %s", c)
	}
}

func TestLifecycle_RoundTripAndEquivalents(t *testing.T) {
	decl := `[
	  {"ID": "logs", "Status": "Enabled", "Filter": {"And": {"Prefix": "logs/", "Tag": [{"Key": "k", "Value": "v"}, {"Key": "a", "Value": "b"}]}},
	   "Transition": [{"Days": 30, "StorageClass": "GLACIER"}, {"Days": 7, "StorageClass": "STANDARD_IA"}],
	   "Expiration": {"Days": 365}},
	  {"ID": "mpu", "Status": "Disabled", "Filter": {}, "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 3}},
	  {"ID": "date", "Status": "Enabled", "Expiration": {"Date": "2027-01-01T00:00:00Z"}}
	]`
	rules, err := ParseLifecycle(decl)
	if err != nil {
		t.Fatal(err)
	}
	want := canonRules(rules)
	// XML にして読み戻しても同じ正規形。
	back, err := lifecycleFromXML(lifecycleXML(rules))
	if err != nil {
		t.Fatal(err)
	}
	if got := canonRules(back); got != want {
		t.Errorf("round trip:\n got %s\nwant %s", got, want)
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
	if got := canonRules(sr); got != want {
		t.Errorf("server form:\n got %s\nwant %s", got, want)
	}
	if c, _ := CanonLifecycle("[]"); c != "" {
		t.Errorf("empty lifecycle = %q", c)
	}
	if _, err := ParseLifecycle(`[{"ID":"a","Status":"Enabled","Bad Name":1}]`); err == nil {
		t.Error("a key that is not an XML name was accepted")
	}
}

func TestCanonTags(t *testing.T) {
	a, _ := CanonTags(`{"b":"2","a":"1"}`)
	b := CanonTagMap(map[string]string{"a": "1", "b": "2"})
	if a != b || a != `{"a":"1","b":"2"}` {
		t.Errorf("tags: %q vs %q", a, b)
	}
	if c, _ := CanonTags("{}"); c != "" {
		t.Errorf("empty tags = %q", c)
	}
}
