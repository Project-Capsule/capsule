package install

import (
	"encoding/binary"
	"os"
	"testing"
)

func TestLayoutForDisk(t *testing.T) {
	tests := []struct {
		name      string
		sizeBytes uint64
		wantPerm  uint32
		wantErr   bool
	}{
		{
			name:      "16 GiB disk leaves usable PERM",
			sizeBytes: 16 * 1024 * 1024 * 1024,
			// 16 GiB - (1 MiB align + 256 MiB boot + 2*2048 MiB slots) = ~11.5 GiB PERM
			// in 512-byte sectors: ~24M sectors
			wantPerm: 0, // checked below as >= 2 GiB
		},
		{
			name:      "512 GB SSD",
			sizeBytes: 512 * 1000 * 1000 * 1000,
			wantPerm:  0,
		},
		{
			name:      "tiny 1 GiB disk fails",
			sizeBytes: 1 * 1024 * 1024 * 1024,
			wantErr:   true,
		},
		{
			name:      "exactly boot + 2*slot + 2 GiB PERM = ok",
			sizeBytes: (1 + 256 + 2*2048 + 2048) * 1024 * 1024,
			wantPerm:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, err := LayoutForDisk(tt.sizeBytes)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			// Sanity: partitions are contiguous and non-overlapping.
			if l.BootStart != bootPartStart {
				t.Errorf("boot start = %d, want %d", l.BootStart, bootPartStart)
			}
			if l.SlotAStart != l.BootStart+l.BootSectors {
				t.Errorf("slot_a start (%d) != boot end (%d)", l.SlotAStart, l.BootStart+l.BootSectors)
			}
			if l.SlotBStart != l.SlotAStart+l.SlotASectors {
				t.Errorf("slot_b not contiguous with slot_a")
			}
			if l.PermStart != l.SlotBStart+l.SlotBSectors {
				t.Errorf("perm not contiguous with slot_b")
			}
			// PERM must fit in the disk.
			permEndBytes := (uint64(l.PermStart) + uint64(l.PermSectors)) * 512
			if permEndBytes > tt.sizeBytes {
				t.Errorf("layout overshoots disk: ends at %d, disk is %d", permEndBytes, tt.sizeBytes)
			}
			// PERM must be at least the floor.
			permMiB := l.PermSectors / (1024 * 2)
			if permMiB < minPermSizeMiB {
				t.Errorf("perm %d MiB < floor %d MiB", permMiB, minPermSizeMiB)
			}
		})
	}
}

func TestWriteMBR(t *testing.T) {
	// Stand-in block device. 8 GiB is the smallest size LayoutForDisk
	// accepts with the default slot sizes.
	const fakeDiskBytes = 8 * 1024 * 1024 * 1024
	f, err := os.CreateTemp("", "mbr-test-*.img")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if err := f.Truncate(int64(fakeDiskBytes)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	layout, err := LayoutForDisk(fakeDiskBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteMBR(f.Name(), layout); err != nil {
		t.Fatal(err)
	}

	// Only the first 512 bytes are interesting — read just those so the
	// test doesn't materialize the rest of the sparse file.
	rf, err := os.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	mbr := make([]byte, 512)
	if _, err := rf.ReadAt(mbr, 0); err != nil {
		t.Fatal(err)
	}
	rf.Close()

	// Disk signature.
	gotSig := binary.LittleEndian.Uint32(mbr[0x1B8:0x1BC])
	if gotSig != CapsuleMBRSig {
		t.Errorf("disk sig = 0x%08x, want 0x%08x", gotSig, CapsuleMBRSig)
	}
	// Boot signature.
	if mbr[0x1FE] != 0x55 || mbr[0x1FF] != 0xAA {
		t.Errorf("missing 0x55AA boot signature: got %02x %02x", mbr[0x1FE], mbr[0x1FF])
	}

	// Partition 1 = ESP, bootable.
	p1 := mbr[0x1BE:]
	if p1[0] != 0x80 {
		t.Errorf("p1 boot flag = %#x, want 0x80", p1[0])
	}
	if p1[4] != partTypeESP {
		t.Errorf("p1 type = %#x, want %#x", p1[4], partTypeESP)
	}
	gotStart := binary.LittleEndian.Uint32(p1[8:12])
	if gotStart != bootPartStart {
		t.Errorf("p1 start = %d, want %d", gotStart, bootPartStart)
	}

	// Partition 4 = LVM, non-bootable.
	p4 := mbr[0x1BE+3*16:]
	if p4[0] != 0x00 {
		t.Errorf("p4 boot flag = %#x, want 0x00", p4[0])
	}
	if p4[4] != partTypeLVM {
		t.Errorf("p4 type = %#x, want %#x", p4[4], partTypeLVM)
	}
}

func TestPartitionPath(t *testing.T) {
	tests := []struct {
		disk string
		n    int
		want string
	}{
		{"/dev/sda", 1, "/dev/sda1"},
		{"/dev/sda", 4, "/dev/sda4"},
		{"/dev/nvme0n1", 1, "/dev/nvme0n1p1"},
		{"/dev/nvme0n1", 4, "/dev/nvme0n1p4"},
		{"/dev/mmcblk0", 2, "/dev/mmcblk0p2"},
		{"/dev/vda", 3, "/dev/vda3"},
		{"", 1, ""},
	}
	for _, tt := range tests {
		t.Run(tt.disk, func(t *testing.T) {
			if got := partitionPath(tt.disk, tt.n); got != tt.want {
				t.Errorf("partitionPath(%q, %d) = %q, want %q", tt.disk, tt.n, got, tt.want)
			}
		})
	}
}
