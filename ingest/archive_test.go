package ingest

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDetectFormatRouting(t *testing.T) {
	docx := fixtureBytes(t, "not_docx.docx") // ZIP 但不含 word/document.xml
	if got := DetectFormat("x.csv", "text/csv", []byte("a,b\n1,2\n")); got != FormatCSV {
		t.Fatalf("csv text/csv = %q", got)
	}
	if got := DetectFormat("x.csv", "application/vnd.ms-excel", []byte("a,b\n1,2\n")); got != FormatCSV {
		t.Fatalf("csv ms-excel = %q", got)
	}
	if got := DetectFormat("x.csv", "", []byte("a,b\n1,2\n")); got != FormatCSV {
		t.Fatalf("csv no mime = %q", got)
	}
	if got := DetectFormat("x.xlsx", "", fixtureBytes(t, "clean.xlsx")); got != FormatXLSX {
		t.Fatalf("xlsx = %q", got)
	}
	if got := DetectFormat("x.zip", "application/zip", fixtureBytes(t, "store-docx.zip")); got != FormatZIP {
		t.Fatalf("zip extension must win = %q", got)
	}
	if got := DetectFormat("x.zip", "", fixtureBytes(t, "safe.zip")); got != FormatZIP {
		t.Fatalf("generic zip = %q", got)
	}
	if got := DetectFormat("noext", "", []byte("%PDF-1.7")); got != FormatPDF {
		t.Fatalf("pdf sniff = %q", got)
	}
	if got := DetectFormat("noext", "", docx); got != FormatZIP {
		t.Fatalf("generic zip sniff = %q", got)
	}
	// Store 方式收入 .docx 的招标包：中央目录含 word/document.xml，应识别为 DOCX。
	storeDocx := fixtureBytes(t, "store-docx.zip")
	if got := DetectFormat("unknown.zip", "", storeDocx); got != FormatZIP {
		t.Fatalf(".zip always archive = %q", got)
	}
	for _, name := range []string{"a.xls", "a.xlsm", "a.doc", "a.docm", "a.wps", "a.rtf"} {
		if got := DetectFormat(name, "", nil); got != "" {
			t.Fatalf("%s must be unsupported, got %q", name, got)
		}
	}
}

func TestParseRejectsZIPAndInvalidDOCX(t *testing.T) {
	zipData := fixtureBytes(t, "safe.zip")
	if _, err := Parse(context.Background(), File{Name: "safe.zip", Data: zipData}, ParseOptions{}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("zip Parse error = %v", err)
	}
	if _, err := Parse(context.Background(), File{Name: "not_docx.docx", Data: fixtureBytes(t, "not_docx.docx")}, ParseOptions{}); !errors.Is(err, ErrUnsupportedFormat) ||
		!strings.Contains(err.Error(), "docx_missing_document_xml") {
		t.Fatalf("invalid docx error = %v", err)
	}
	if _, err := Parse(context.Background(), File{Name: "legacy.xls", Data: []byte("x")}, ParseOptions{}); !errors.Is(err, ErrUnsupportedFormat) ||
		!strings.Contains(err.Error(), ".xls") {
		t.Fatalf("legacy xls error = %v", err)
	}
}

func TestParseArchiveSafeAndBoundaries(t *testing.T) {
	arch, err := ParseArchive(fixtureBytes(t, "safe.zip"), DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(arch.Entries) != 4 || arch.TotalBytes == 0 {
		t.Fatalf("safe archive = %+v", arch)
	}
	want := map[string]string{"招标文件.docx": FormatDOCX, "附件/报价清单.xlsx": FormatXLSX, "附件/数据.csv": FormatCSV, "说明.txt": FormatText}
	for _, e := range arch.Entries {
		if want[e.Path] != e.Format {
			t.Fatalf("entry %s format = %q want %q", e.Path, e.Format, want[e.Path])
		}
		if len(e.SHA256) != 64 {
			t.Fatalf("entry %s sha = %q", e.Path, e.SHA256)
		}
	}

	var childDOCX []byte
	stored, err := WalkArchive(fixtureBytes(t, "store-docx.zip"), DefaultArchiveLimits(), func(e *ArchiveEntry, data []byte) error {
		if e.Path == "标包.docx" {
			childDOCX = append([]byte(nil), data...)
		}
		return nil
	})
	if err != nil || len(stored.Entries) != 1 || stored.Entries[0].Format != FormatDOCX {
		t.Fatalf("store docx archive = %+v %v", stored, err)
	}
	if got := DetectFormat("noext", "", childDOCX); got != FormatDOCX {
		t.Fatalf("DOCX central-directory sniff = %q", got)
	}

	cases := []struct {
		file   string
		limits ArchiveLimits
		code   string
	}{
		{"zip-traversal.zip", ArchiveLimits{}, ArchivePathTraversal},
		{"zip-absolute.zip", ArchiveLimits{}, ArchiveAbsolutePath},
		{"zip-drive.zip", ArchiveLimits{}, ArchiveDrivePath},
		{"zip-backslash.zip", ArchiveLimits{}, ArchiveBackslash},
		{"zip-symlink.zip", ArchiveLimits{}, ArchiveSymlink},
		{"zip-duplicate.zip", ArchiveLimits{}, ArchiveDuplicatePath},
		{"zip-nested.zip", ArchiveLimits{}, ArchiveNested},
		{"zip-ratio.zip", ArchiveLimits{}, ArchiveRatioLimit},
		{"zip-many.zip", ArchiveLimits{}, ArchiveEntryLimit},
		{"zip-oversize.zip", ArchiveLimits{MaxEntryBytes: 65536}, ArchiveEntrySizeLimit},
	}
	for _, tc := range cases {
		_, err := ParseArchive(fixtureBytes(t, tc.file), tc.limits)
		var ae *ArchiveError
		if !errors.As(err, &ae) || ae.Code != tc.code {
			t.Fatalf("%s error = %v (want code %s)", tc.file, err, tc.code)
		}
	}
}

func TestWalkArchiveCallbackAndTotalLimit(t *testing.T) {
	var paths []string
	arch, err := WalkArchive(fixtureBytes(t, "safe.zip"), DefaultArchiveLimits(), func(e *ArchiveEntry, data []byte) error {
		paths = append(paths, e.Path)
		if len(data) != int(e.Size) {
			t.Fatalf("entry %s size mismatch", e.Path)
		}
		return nil
	})
	if err != nil || len(paths) != 4 || len(arch.Entries) != 4 {
		t.Fatalf("walk = %v %v", paths, err)
	}
	_, err = ParseArchive(fixtureBytes(t, "safe.zip"), ArchiveLimits{MaxTotalBytes: 1024})
	var ae *ArchiveError
	if !errors.As(err, &ae) || ae.Code != ArchiveTotalSizeLimit {
		t.Fatalf("total limit error = %v", err)
	}
}
