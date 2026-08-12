package fragmentation

import (
	"strconv"
	"strings"
	"testing"
)

func TestParseXFSBmapReport(t *testing.T) {
	output := `EXT: FILE-OFFSET      BLOCK-RANGE      AG AG-OFFSET        TOTAL FLAGS
   0: [0..2047]:       1000..3047       0 (1000..3047)      2048 000000
   1: [2048..4095]:    hole                                  2048
   2: [4096..4103]:    9000..9007       0 (9000..9007)         8 000000
`
	report, err := Parse(output)
	if err != nil {
		t.Fatal(err)
	}
	if report.ExtentCount != 2 || report.TinyExtentCount != 1 {
		t.Fatalf("extent counts = %d/%d", report.ExtentCount, report.TinyExtentCount)
	}
	if report.AllocatedBytes != (2048+8)*512 || report.HoleBytes != 2048*512 {
		t.Fatalf("bytes = allocated %d, holes %d", report.AllocatedBytes, report.HoleBytes)
	}
	if report.LargestExtentBytes != 2048*512 || report.Severity != SeverityHealthy {
		t.Fatalf("report = %#v", report)
	}
}

func TestClassifyRequiresEnoughExtentsBeforeUsingTinyExtentRatio(t *testing.T) {
	t.Parallel()
	freshImage := Report{ExtentCount: 16, TinyExtentCount: 2}
	classify(&freshImage)
	if freshImage.Severity != SeverityHealthy {
		t.Fatalf("fresh image severity = %s", freshImage.Severity)
	}

	fragmentedImage := Report{ExtentCount: 129, TinyExtentCount: 20}
	classify(&fragmentedImage)
	if fragmentedImage.Severity != SeverityWarning {
		t.Fatalf("fragmented image severity = %s", fragmentedImage.Severity)
	}
}

func TestParseRejectsMissingExtents(t *testing.T) {
	_, err := Parse("EXT: FILE-OFFSET BLOCK-RANGE\n")
	if err == nil || !strings.Contains(err.Error(), "未返回") {
		t.Fatalf("Parse() error = %v", err)
	}
}

func TestParseClassifiesLargeFragmentCount(t *testing.T) {
	var output strings.Builder
	for index := 0; index < 4097; index++ {
		start := index * 8
		output.WriteString("0: [")
		output.WriteString(strconv.Itoa(start))
		output.WriteString("..")
		output.WriteString(strconv.Itoa(start + 7))
		output.WriteString("]: 1..8 0 (1..8) 8 000000\n")
	}
	report, err := Parse(output.String())
	if err != nil {
		t.Fatal(err)
	}
	if report.Severity != SeverityCritical {
		t.Fatalf("severity = %s", report.Severity)
	}
}
