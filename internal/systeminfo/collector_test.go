package systeminfo

import "testing"

func TestParseCPUSampleAndCalculateUsage(t *testing.T) {
	t.Parallel()
	previous, err := parseCPUSample("cpu  100 10 50 800 20 5 5 10 0 0\n")
	if err != nil {
		t.Fatal(err)
	}
	current, err := parseCPUSample("cpu  130 10 70 850 20 5 5 10 0 0\n")
	if err != nil {
		t.Fatal(err)
	}
	usage := calculateCPUUsage(&previous, current)
	if usage == nil || *usage != 50 {
		t.Fatalf("usage = %v, want 50", usage)
	}
}

func TestParseCPUSampleRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	if _, err := parseCPUSample("intr 1 2 3\n"); err == nil {
		t.Fatal("parseCPUSample() error = nil")
	}
}

func TestNewUsage(t *testing.T) {
	t.Parallel()
	usage := newUsage(1000, 250)
	if usage.UsedBytes != 750 || usage.UsedPercent != 75 {
		t.Fatalf("usage = %#v", usage)
	}
}
