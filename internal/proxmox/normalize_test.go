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

package proxmox

import (
	"strings"
	"testing"
)

func TestSize(t *testing.T) {
	for in, want := range map[string]string{
		"32G": "32G", "32768M": "32G", "33554432K": "32G", "1024G": "1T", "1536M": "1536M", "512": "512", "0.5G": "512M",
	} {
		b, err := ParseSize(in)
		if err != nil || FormatSize(b) != want {
			t.Errorf("%s → %s, %v; want %s", in, FormatSize(b), err, want)
		}
	}
	if _, err := ParseSize("32GB"); err == nil {
		t.Error("32GB parsed")
	}
}

func TestMemory(t *testing.T) {
	for in, want := range map[string]int{"2048": 2048, "2048M": 2048, "2G": 2048, "1T": 1 << 20} {
		if got, err := ParseMemory(in); err != nil || got != want {
			t.Errorf("ParseMemory(%s) = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0", "-1", "1.5K", "abc"} {
		if _, err := ParseMemory(bad); err == nil {
			t.Errorf("ParseMemory(%q) succeeded", bad)
		}
	}
	for raw, want := range map[string]string{"": "512", "4096": "4096", "current=8192": "8192", "current=8192,max=16384": "8192"} {
		if got := observedMemory(raw); got != want {
			t.Errorf("observedMemory(%q) = %s", raw, got)
		}
	}
}

func TestDisks(t *testing.T) {
	ds, err := ParseDisks(" scsi10 storage=fast size=1024G,scsi2 size=32768M storage=local-lvm ")
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatDisks(ds); got != "scsi2 storage=local-lvm size=32G, scsi10 storage=fast size=1T" {
		t.Errorf("FormatDisks = %q", got)
	}
	for _, bad := range []string{"net0 storage=a size=1G", "scsi0 storage=a", "scsi0 storage=a size=1536M", "scsi0 storage=a size=1G, scsi0 storage=b size=2G", "scsi0 storage=a size=1G pool=x", "scsi0 storage"} {
		if _, err := ParseDisks(bad); err == nil {
			t.Errorf("ParseDisks(%q) succeeded", bad)
		}
	}
	for raw, want := range map[string]string{
		"local-lvm:vm-100-disk-0,iothread=1,size=32G":  "scsi0 storage=local-lvm size=32G",
		"ceph:vm-100-disk-1,size=33554432K,discard=on": "scsi0 storage=ceph size=32G",
	} {
		d, ok := diskFromConfig("scsi0", raw)
		if !ok || d.String() != want {
			t.Errorf("diskFromConfig(%q) = %q, %v", raw, d, ok)
		}
	}
	for _, raw := range []string{"local:iso/debian.iso,media=cdrom", "none,media=cdrom", "local-lvm:vm-100-cloudinit,media=cdrom", "/dev/sdb"} {
		if _, ok := diskFromConfig("ide2", raw); ok {
			t.Errorf("diskFromConfig(%q) is a disk", raw)
		}
	}
}

func TestNICs(t *testing.T) {
	ns, err := ParseNICs("net1 bridge=vmbr1, net0 bridge=vmbr0 vlan=10 mac=BC:24:11:00:00:01 model=virtio")
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatNICs(ns); got != "net0 bridge=vmbr0 vlan=10 mac=bc:24:11:00:00:01 model=virtio, net1 bridge=vmbr1" {
		t.Errorf("FormatNICs = %q", got)
	}
	for _, bad := range []string{"net0 vlan=10", "eth0 bridge=vmbr0", "net0 bridge=vmbr0 vlan=4095", "net0 bridge=vmbr0 mac=xyz", "net0 bridge=vmbr0 model=ne2k"} {
		if _, err := ParseNICs(bad); err == nil {
			t.Errorf("ParseNICs(%q) succeeded", bad)
		}
	}
	n := nicFromConfig("net0", "virtio=BC:24:11:AA:BB:CC,bridge=vmbr0,firewall=1,tag=20")
	if n.String() != "net0 bridge=vmbr0 vlan=20 mac=bc:24:11:aa:bb:cc model=virtio" {
		t.Errorf("nicFromConfig = %q", n)
	}
	// 宣言しない MAC・型と、ほかのキー（firewall）は残す。タグは宣言どおり（無ければ外す）。
	if got := nicConfig(NIC{Slot: "net0", Bridge: "vmbr1"}, "e1000=BC:24:11:AA:BB:CC,bridge=vmbr0,firewall=1,tag=20"); got != "e1000=BC:24:11:AA:BB:CC,bridge=vmbr1,firewall=1" {
		t.Errorf("nicConfig = %q", got)
	}
	if got := nicConfig(NIC{Slot: "net0", Bridge: "vmbr0", VLAN: 5}, ""); got != "virtio,bridge=vmbr0,tag=5" {
		t.Errorf("nicConfig (new) = %q", got)
	}
}

func TestCloudInit(t *testing.T) {
	keys := "ssh-ed25519 AAAAC3Nza+/x= ops@example\n\n  ssh-rsa AAAAB3 other  \n"
	canon := CanonSSHKeys(keys)
	if canon != "ssh-ed25519 AAAAC3Nza+/x= ops@example\nssh-rsa AAAAB3 other" {
		t.Errorf("CanonSSHKeys = %q", canon)
	}
	enc := encodeURIComponent(canon + "\n")
	if strings.ContainsAny(enc, " +/=\n") {
		t.Errorf("encodeURIComponent left reserved characters: %q", enc)
	}
	if got := observedSSHKeys(enc); got != canon {
		t.Errorf("observedSSHKeys = %q", got)
	}
	good := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHVhH2ihtRkDaQdDLczl6sw3NLY3l1cwGyKEuzW9kt4F sakutei@ctl\n" +
		`from="10.0.0.0/8" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHVhH2ihtRkDaQdDLczl6sw3NLY3l1cwGyKEuzW9kt4F`
	if err := CheckSSHKeys(good); err != nil {
		t.Errorf("CheckSSHKeys(good) = %v", err)
	}
	for _, bad := range []string{"not a key", "ssh-ed25519", "ssh-ed25519 !!!", "ssh-rsa AAAAC3NzaC1lZDI1NTE5AAAAIHVhH2ihtRkDaQdDLczl6sw3NLY3l1cwGyKEuzW9kt4F"} {
		if err := CheckSSHKeys(bad); err == nil {
			t.Errorf("CheckSSHKeys(%q) succeeded", bad)
		}
	}
	if got, _ := CanonDNS("192.0.2.53,  192.0.2.54"); got != "192.0.2.53 192.0.2.54" {
		t.Errorf("CanonDNS = %q", got)
	}
	if _, err := CanonDNS("dns.example"); err == nil {
		t.Error("CanonDNS accepted a name")
	}
	if got, _ := CanonIP("192.0.2.10/24"); got != "192.0.2.10/24" {
		t.Errorf("CanonIP = %q", got)
	}
	if _, err := CanonIP("192.0.2.10"); err == nil {
		t.Error("CanonIP accepted an address without a prefix length")
	}
	if got := ipconfigValue("ip=dhcp,ip6=auto", "192.0.2.10/24", true, "192.0.2.1", true); got != "ip=192.0.2.10/24,gw=192.0.2.1,ip6=auto" {
		t.Errorf("ipconfigValue = %q", got)
	}
	if got := ipconfigValue("ip=192.0.2.10/24,gw=192.0.2.1", "dhcp", true, "", false); got != "ip=dhcp" {
		t.Errorf("ipconfigValue (dhcp) = %q", got)
	}
}
