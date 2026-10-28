package main

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
)

const fixtureBase = 0x400000

// Fixtures are constructed in memory, so tests work on Windows without a Go
// subprocess, C compiler, Linux loader, network access, or executable candidate.
type fixtureOptions struct {
	info               debug.BuildInfo
	buildInfoText      string
	kind               elf.Type
	machine            elf.Machine
	abi                elf.OSABI
	entry              uint64
	interpreter        string
	libraries          []string
	versions           []string
	omitBuildInfo      bool
	omitVersionNeeds   bool
	omitVersionSymbols bool
	truncateNeeds      bool
}

func validFixtureOptions() fixtureOptions {
	return fixtureOptions{
		info: debug.BuildInfo{
			GoVersion: "go1.27.0",
			Settings: []debug.BuildSetting{
				{Key: "CGO_ENABLED", Value: "1"},
				{Key: "GOOS", Value: "linux"},
				{Key: "GOARCH", Value: "amd64"},
				{Key: "GOAMD64", Value: "v1"},
				{Key: "-buildmode", Value: "exe"},
			},
		},
		kind:        elf.ET_EXEC,
		machine:     elf.EM_X86_64,
		entry:       fixtureBase + 0x100,
		interpreter: "/lib64/ld-linux-x86-64.so.2\x00",
		libraries:   []string{"libc.so.6"},
		versions:    []string{"GLIBC_2.2.5", "GLIBC_2.35"},
	}
}

func setFixtureSetting(f *fixtureOptions, key, value string) {
	for i := range f.info.Settings {
		if f.info.Settings[i].Key == key {
			f.info.Settings[i].Value = value
			return
		}
	}
	f.info.Settings = append(f.info.Settings, debug.BuildSetting{Key: key, Value: value})
}

func removeFixtureSetting(f *fixtureOptions, key string) {
	for i := range f.info.Settings {
		if f.info.Settings[i].Key == key {
			f.info.Settings = append(f.info.Settings[:i], f.info.Settings[i+1:]...)
			return
		}
	}
}

func fixtureELF(t *testing.T, options fixtureOptions) []byte {
	t.Helper()
	order := binary.LittleEndian
	encode := func(value any) []byte {
		t.Helper()
		var out bytes.Buffer
		if errWrite := binary.Write(&out, order, value); errWrite != nil {
			t.Fatal(errWrite)
		}
		return out.Bytes()
	}
	type section struct {
		name   string
		data   []byte
		header elf.Section64
	}
	sections := []section{{}}
	add := func(name string, kind elf.SectionType, data []byte, link, info uint32, entsize uint64) uint32 {
		sections = append(sections, section{
			name: name,
			data: data,
			header: elf.Section64{
				Type: uint32(kind), Link: link, Info: info, Entsize: entsize, Addralign: 16,
			},
		})
		return uint32(len(sections) - 1)
	}
	add(".text", elf.SHT_PROGBITS, []byte{0xc3}, 0, 0, 0)
	interp := add(".interp", elf.SHT_PROGBITS, []byte(options.interpreter), 0, 0, 0)
	if !options.omitBuildInfo {
		header := make([]byte, 32)
		copy(header, "\xff Go buildinf:")
		header[14], header[15] = 8, 2
		appendString := func(text string) {
			header = binary.AppendUvarint(header, uint64(len(text)))
			header = append(header, text...)
		}
		appendString(options.info.GoVersion)
		text := options.info.String()
		if options.buildInfoText != "" {
			text = options.buildInfoText
		}
		appendString(strings.Repeat("x", 16) + text + strings.Repeat("y", 16))
		add(".go.buildinfo", elf.SHT_PROGBITS, header, 0, 0, 0)
	}
	dynStrings := []byte{0}
	stringOffset := func(text string) uint32 {
		offset := uint32(len(dynStrings))
		dynStrings = append(dynStrings, text...)
		dynStrings = append(dynStrings, 0)
		return offset
	}
	var dynamic []elf.Dyn64
	for _, library := range options.libraries {
		dynamic = append(dynamic, elf.Dyn64{Tag: int64(elf.DT_NEEDED), Val: uint64(stringOffset(library))})
	}
	dynamic = append(dynamic, elf.Dyn64{Tag: int64(elf.DT_NULL)})
	libc := stringOffset("libc.so.6")
	var offsets []uint32
	for _, version := range options.versions {
		offsets = append(offsets, stringOffset(version))
	}
	symbolName := stringOffset("example_import")
	dynstr := add(".dynstr", elf.SHT_STRTAB, dynStrings, 0, 0, 0)
	add(".dynamic", elf.SHT_DYNAMIC, encode(dynamic), dynstr, 0, 16)
	dynsym := add(".dynsym", elf.SHT_DYNSYM, encode([]elf.Sym64{
		{}, {Name: symbolName, Info: byte(elf.STB_GLOBAL << 4), Shndx: uint16(elf.SHN_UNDEF)},
	}), dynstr, 1, 24)
	if !options.omitVersionSymbols {
		add(".gnu.version", elf.SHT_GNU_VERSYM, encode([]uint16{0, 2}), dynsym, 0, 2)
	}
	if !options.omitVersionNeeds {
		data := make([]byte, 16+16*len(offsets))
		order.PutUint16(data[0:2], 1)
		order.PutUint16(data[2:4], uint16(len(offsets)))
		order.PutUint32(data[4:8], libc)
		order.PutUint32(data[8:12], 16)
		for i, offset := range offsets {
			entry := data[16+i*16 : 32+i*16]
			order.PutUint16(entry[6:8], uint16(i+2))
			order.PutUint32(entry[8:12], offset)
			if i+1 < len(offsets) {
				order.PutUint32(entry[12:16], 16)
			}
		}
		if options.truncateNeeds {
			data = data[:len(data)-1]
		}
		add(".gnu.version_r", elf.SHT_GNU_VERNEED, data, dynstr, 1, 0)
	}
	shstr := add(".shstrtab", elf.SHT_STRTAB, nil, 0, 0, 0)
	names := []byte{0}
	for i := 1; i < len(sections); i++ {
		sections[i].header.Name = uint32(len(names))
		names = append(names, sections[i].name...)
		names = append(names, 0)
	}
	sections[shstr].data = names
	image := make([]byte, 0x100)
	for i := 1; i < len(sections); i++ {
		for len(image)%16 != 0 {
			image = append(image, 0)
		}
		sections[i].header.Off = uint64(len(image))
		sections[i].header.Addr = fixtureBase + uint64(len(image))
		sections[i].header.Size = uint64(len(sections[i].data))
		image = append(image, sections[i].data...)
	}
	for len(image)%8 != 0 {
		image = append(image, 0)
	}
	sectionOffset := len(image)
	for _, section := range sections {
		image = append(image, encode(section.header)...)
	}
	header := elf.Header64{
		Type: uint16(options.kind), Machine: uint16(options.machine), Version: 1,
		Entry: options.entry, Phoff: 64, Shoff: uint64(sectionOffset), Ehsize: 64,
		Phentsize: 56, Phnum: 2, Shentsize: 64, Shnum: uint16(len(sections)), Shstrndx: uint16(shstr),
	}
	copy(header.Ident[:], "\x7fELF")
	header.Ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	header.Ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	header.Ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	header.Ident[elf.EI_OSABI] = byte(options.abi)
	copy(image, encode(header))
	load := elf.Prog64{
		Type: uint32(elf.PT_LOAD), Flags: uint32(elf.PF_R | elf.PF_W | elf.PF_X),
		Vaddr: fixtureBase, Filesz: uint64(len(image)), Memsz: uint64(len(image)), Align: 0x1000,
	}
	copy(image[64:], encode(load))
	if options.interpreter != "" {
		program := elf.Prog64{
			Type: uint32(elf.PT_INTERP), Flags: uint32(elf.PF_R),
			Off: sections[interp].header.Off, Vaddr: sections[interp].header.Addr,
			Filesz: sections[interp].header.Size, Memsz: sections[interp].header.Size, Align: 1,
		}
		copy(image[120:], encode(program))
	}
	return image
}

func writeFixture(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	return path
}

func mustVersion(t *testing.T, text string) glibcVersion {
	t.Helper()
	v, errParse := parseVersion(text)
	if errParse != nil {
		t.Fatal(errParse)
	}
	return v
}

func TestVersionNumericComparison(t *testing.T) {
	for _, test := range []struct {
		left, right string
		want        int
	}{
		{"2.9", "2.35", -1}, {"2.35", "2.9", 1}, {"2.100", "2.35", 1},
		{"2.2.5", "2.2.12", -1}, {"2.35.1", "2.35", 1}, {"2.35", "2.35.0", 0},
		{"2.35", "2.35", 0}, {"3.0", "2.999", 1},
	} {
		t.Run(test.left+"_vs_"+test.right, func(t *testing.T) {
			if got := compareVersions(mustVersion(t, test.left), mustVersion(t, test.right)); got != test.want {
				t.Fatalf("comparison = %d, want %d", got, test.want)
			}
		})
	}
	for _, invalid := range []string{"", "2", "2.", "2.35.0.1", "2.35suffix", " 2.35", "GLIBC_2.35", "-2.35", "2.+35", "2.18446744073709551616"} {
		if _, errParse := parseVersion(invalid); errParse == nil {
			t.Errorf("accepted invalid version %q", invalid)
		}
	}
}

func TestValidateBuildInfo(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*fixtureOptions)
		want   string
	}{
		{"valid", func(f *fixtureOptions) {}, ""},
		{"no_optional_buildmode", func(f *fixtureOptions) { removeFixtureSetting(f, "-buildmode") }, ""},
		{"cgo_disabled", func(f *fixtureOptions) { setFixtureSetting(f, "CGO_ENABLED", "0") }, "CGO_ENABLED"},
		{"wrong_os", func(f *fixtureOptions) { setFixtureSetting(f, "GOOS", "windows") }, "GOOS"},
		{"wrong_arch", func(f *fixtureOptions) { setFixtureSetting(f, "GOARCH", "arm64") }, "GOARCH"},
		{"wrong_baseline", func(f *fixtureOptions) { setFixtureSetting(f, "GOAMD64", "v3") }, "GOAMD64"},
		{"missing_cgo", func(f *fixtureOptions) { removeFixtureSetting(f, "CGO_ENABLED") }, "missing required build setting CGO_ENABLED"},
		{"missing_os", func(f *fixtureOptions) { removeFixtureSetting(f, "GOOS") }, "missing required build setting GOOS"},
		{"missing_arch", func(f *fixtureOptions) { removeFixtureSetting(f, "GOARCH") }, "missing required build setting GOARCH"},
		{"missing_baseline", func(f *fixtureOptions) { removeFixtureSetting(f, "GOAMD64") }, "missing required build setting GOAMD64"},
		{"empty_baseline", func(f *fixtureOptions) { setFixtureSetting(f, "GOAMD64", "") }, "GOAMD64"},
		{"plugin_not_executable", func(f *fixtureOptions) { setFixtureSetting(f, "-buildmode", "plugin") }, "-buildmode"},
		{"duplicate", func(f *fixtureOptions) {
			f.info.Settings = append(f.info.Settings, debug.BuildSetting{Key: "CGO_ENABLED", Value: "1"})
		}, "duplicate build setting CGO_ENABLED"},
		{"empty_go_version", func(f *fixtureOptions) { f.info.GoVersion = "" }, "Go toolchain version"},
		{"invalid_go_version", func(f *fixtureOptions) { f.info.GoVersion = "not-a-go-version" }, "Go toolchain version"},
		{"custom_version_may_contain_secrets", func(f *fixtureOptions) { f.info.GoVersion = "go1.27.0-private" }, "Go toolchain version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := validFixtureOptions()
			test.mutate(&fixture)
			_, errInfo := validateBuildInfo(&fixture.info)
			if test.want == "" {
				if errInfo != nil {
					t.Fatal(errInfo)
				}
			} else if errInfo == nil || !strings.Contains(errInfo.Error(), test.want) {
				t.Fatalf("error = %v, want %q", errInfo, test.want)
			}
		})
	}
	if _, errInfo := validateBuildInfo(nil); errInfo == nil {
		t.Fatal("accepted nil build information")
	}
}

func TestVerifyValidArtifactJSON(t *testing.T) {
	for _, kind := range []elf.Type{elf.ET_EXEC, elf.ET_DYN} {
		t.Run(kind.String(), func(t *testing.T) {
			fixture := validFixtureOptions()
			fixture.kind = kind
			if kind == elf.ET_DYN {
				setFixtureSetting(&fixture, "-buildmode", "pie")
			}
			fixture.versions = []string{"GLIBC_2.35", "GLIBC_2.9", "GLIBC_2.2.5", "GLIBC_2.35", "GLIBCXX_99.99", "NOT_GLIBC_99.99"}
			fixture.libraries = []string{"libpthread.so.0", "libc.so.6"}
			const secret = "DO_NOT_OUTPUT_TEST_SECRET"
			fixture.info.Path = "private/" + secret
			fixture.info.Settings = append(fixture.info.Settings,
				debug.BuildSetting{Key: "-ldflags", Value: "-X main.secret=" + secret},
				debug.BuildSetting{Key: "CGO_CFLAGS", Value: "-DSECRET=" + secret},
				debug.BuildSetting{Key: "PRIVATE_TOKEN", Value: secret},
			)
			data := fixtureELF(t, fixture)
			path := writeFixture(t, "cli-proxy-api", data)
			var stdout, stderr bytes.Buffer
			// PATH is intentionally empty: verification must not spawn any tool.
			t.Setenv("PATH", "")
			if code := run([]string{path}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
			}
			if stderr.Len() != 0 || strings.Contains(stdout.String(), secret) {
				t.Fatalf("unexpected output: stderr = %q; secret leaked = %t", stderr.String(), strings.Contains(stdout.String(), secret))
			}
			var report artifactReport
			decoder := json.NewDecoder(&stdout)
			decoder.DisallowUnknownFields()
			if errDecode := decoder.Decode(&report); errDecode != nil {
				t.Fatal(errDecode)
			}
			if errDecode := decoder.Decode(new(any)); errDecode != io.EOF {
				t.Fatalf("unexpected trailing output: %v", errDecode)
			}
			digest := sha256.Sum256(data)
			if report.SHA256 != hex.EncodeToString(digest[:]) || report.GoVersion != "go1.27.0" || report.MaxGLIBC != "2.35" {
				t.Fatalf("incorrect artifact identity: %+v", report)
			}
			if report.ELFType != kind.String() || report.Interpreter != "/lib64/ld-linux-x86-64.so.2" {
				t.Fatalf("incorrect ELF metadata: %+v", report)
			}
			if !reflect.DeepEqual(report.Dependencies, []string{"libc.so.6", "libpthread.so.0"}) ||
				!reflect.DeepEqual(report.GLIBCVersions, []string{"GLIBC_2.2.5", "GLIBC_2.9", "GLIBC_2.35"}) || report.MaxRequiredGLIBC != "GLIBC_2.35" {
				t.Fatalf("incorrect dependency metadata: %+v", report)
			}
			if len(report.Settings) != 5 || report.Settings["CGO_ENABLED"] != "1" || report.Settings["GOAMD64"] != "v1" {
				t.Fatalf("incorrect filtered settings: %v", report.Settings)
			}
		})
	}
}

func TestRejectArtifact(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*fixtureOptions)
		want   string
	}{
		{"cgo_disabled", func(f *fixtureOptions) { setFixtureSetting(f, "CGO_ENABLED", "0") }, "CGO_ENABLED"},
		{"wrong_os", func(f *fixtureOptions) { setFixtureSetting(f, "GOOS", "darwin") }, "GOOS"},
		{"wrong_arch", func(f *fixtureOptions) { setFixtureSetting(f, "GOARCH", "arm64") }, "GOARCH"},
		{"missing_goamd64", func(f *fixtureOptions) { removeFixtureSetting(f, "GOAMD64") }, "GOAMD64"},
		{"wrong_elf_machine", func(f *fixtureOptions) { f.machine = elf.EM_AARCH64 }, "x86_64"},
		{"wrong_elf_abi", func(f *fixtureOptions) { f.abi = elf.ELFOSABI_FREEBSD }, "ABI"},
		{"relocatable", func(f *fixtureOptions) { f.kind = elf.ET_REL }, "executable or PIE"},
		{"no_entry", func(f *fixtureOptions) { f.entry = 0 }, "entry point"},
		{"unmapped_entry", func(f *fixtureOptions) { f.entry = 0x700000 }, "entry point"},
		{"missing_interpreter", func(f *fixtureOptions) { f.interpreter = "" }, "PT_INTERP"},
		{"relative_interpreter", func(f *fixtureOptions) { f.interpreter = "ld-linux.so\x00" }, "PT_INTERP"},
		{"unterminated_interpreter", func(f *fixtureOptions) { f.interpreter = "/lib/ld-linux.so" }, "PT_INTERP"},
		{"embedded_nul_interpreter", func(f *fixtureOptions) { f.interpreter = "/lib/ld\x00extra\x00" }, "PT_INTERP"},
		{"missing_buildinfo", func(f *fixtureOptions) { f.omitBuildInfo = true }, "Go build information"},
		{"mismatched_pie", func(f *fixtureOptions) { setFixtureSetting(f, "-buildmode", "pie") }, "does not match ELF"},
		{"no_dynamic_dependencies", func(f *fixtureOptions) { f.libraries = nil }, "DT_NEEDED"},
		{"empty_dependency", func(f *fixtureOptions) { f.libraries = []string{""} }, "dependency"},
		{"missing_version_need", func(f *fixtureOptions) { f.omitVersionNeeds = true }, "version-need"},
		{"missing_version_symbols", func(f *fixtureOptions) { f.omitVersionSymbols = true }, "version requirements"},
		{"truncated_version_need", func(f *fixtureOptions) { f.truncateNeeds = true }, "truncated"},
		{"no_version_requirements", func(f *fixtureOptions) { f.versions = nil }, "version-need"},
		{"no_glibc_requirements", func(f *fixtureOptions) { f.versions = []string{"GLIBCXX_3.4.99"} }, "no exact numeric GLIBC"},
		{"glibc_too_new", func(f *fixtureOptions) { f.versions = []string{"GLIBC_2.36"} }, "GLIBC_2.36 exceeds --max-glibc 2.35"},
		{"glibc_numeric_not_lexical", func(f *fixtureOptions) { f.versions = []string{"GLIBC_2.100"} }, "GLIBC_2.100 exceeds"},
		{"glibc_patch_too_new", func(f *fixtureOptions) { f.versions = []string{"GLIBC_2.35.1"} }, "GLIBC_2.35.1 exceeds"},
		{"glibc_private", func(f *fixtureOptions) { f.versions = []string{"GLIBC_2.2.5", "GLIBC_PRIVATE"} }, "nonnumeric GLIBC"},
		{"glibc_not_partial_match", func(f *fixtureOptions) { f.versions = []string{"GLIBC_2.35suffix"} }, "nonnumeric GLIBC"},
		{"glibc_abi_feature", func(f *fixtureOptions) { f.versions = []string{"GLIBC_ABI_DT_RELR"} }, "nonnumeric GLIBC"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := validFixtureOptions()
			test.mutate(&fixture)
			path := writeFixture(t, "candidate", fixtureELF(t, fixture))
			var stdout, stderr bytes.Buffer
			if code := run([]string{path}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit = %d, want 1; stderr = %s", code, stderr.String())
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("stdout = %q; stderr = %q; want %q", stdout.String(), stderr.String(), test.want)
			}
		})
	}
}

func TestRejectNonELFAndNoPlugin(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("not an ELF\n"), []byte("\x7fELF\x02"), []byte("MZ Windows executable")} {
		path := writeFixture(t, "candidate", data)
		if _, errVerify := verifyArtifact(path, mustVersion(t, "2.35")); errVerify == nil {
			t.Fatalf("accepted non-ELF input %q", data)
		}
	}
	valid := fixtureELF(t, validFixtureOptions())
	for _, name := range []string{"cli-proxy-api_no-plugin", "CLIProxyAPI_NO-PLUGIN", "cli_no_plugin", "cli-noplugin"} {
		path := writeFixture(t, name, valid)
		if _, errVerify := verifyArtifact(path, mustVersion(t, "2.35")); errVerify == nil || !strings.Contains(errVerify.Error(), "filename marks a no-plugin") {
			t.Fatalf("no-plugin error = %v", errVerify)
		}
	}
	for _, path := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		if _, errVerify := verifyArtifact(path, mustVersion(t, "2.35")); errVerify == nil {
			t.Fatal("accepted missing or nonregular file")
		}
	}
}

func TestCLIOptions(t *testing.T) {
	path := writeFixture(t, "candidate", fixtureELF(t, validFixtureOptions()))
	for _, test := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"explicit_ceiling", []string{"--max-glibc", "2.35", path}, 0, ""},
		{"higher_ceiling", []string{"--max-glibc=2.100", path}, 0, ""},
		{"lower_ceiling", []string{"--max-glibc", "2.34", path}, 1, "exceeds"},
		{"invalid_ceiling", []string{"--max-glibc", "2.35junk", path}, 2, "invalid --max-glibc"},
		{"missing_ceiling", []string{"--max-glibc"}, 2, "invalid command-line options"},
		{"no_file", nil, 2, "usage:"},
		{"extra_file", []string{path, path}, 2, "usage:"},
		{"empty_file", []string{""}, 2, "usage:"},
		{"unknown_option", []string{"--unknown"}, 2, "invalid command-line options"},
		{"help", []string{"--help"}, 0, "usage:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(test.args, &stdout, &stderr); code != test.code {
				t.Fatalf("exit = %d, want %d; stderr = %s", code, test.code, stderr.String())
			}
			if !strings.Contains(stderr.String(), test.want) || (test.code != 0 && stdout.Len() != 0) {
				t.Fatalf("stdout = %q; stderr = %q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestErrorsDoNotExposeBuildSecrets(t *testing.T) {
	const secret = "DO_NOT_OUTPUT_ERROR_TEST_SECRET"
	fixture := validFixtureOptions()
	setFixtureSetting(&fixture, "CGO_ENABLED", secret)
	fixture.info.Settings = append(fixture.info.Settings, debug.BuildSetting{Key: "-ldflags", Value: secret})
	path := writeFixture(t, "candidate", fixtureELF(t, fixture))
	var stdout, stderr bytes.Buffer
	if code := run([]string{path}, &stdout, &stderr); code != 1 || strings.Contains(stderr.String(), secret) || stdout.Len() != 0 {
		t.Fatalf("unsafe failure output: stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	fixture = validFixtureOptions()
	fixture.buildInfoText = fixture.info.String() + "build\tPRIVATE=unquoted " + secret + "\n"
	path = writeFixture(t, "malformed", fixtureELF(t, fixture))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{path}, &stdout, &stderr); code != 1 || strings.Contains(stderr.String(), secret) || stdout.Len() != 0 || !strings.Contains(stderr.String(), "cannot read Go build information") {
		t.Fatalf("unsafe parser failure: stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	fixture = validFixtureOptions()
	fixture.info.GoVersion += "-" + secret
	path = writeFixture(t, "custom-toolchain", fixtureELF(t, fixture))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{path}, &stdout, &stderr); code != 1 || strings.Contains(stderr.String(), secret) || stdout.Len() != 0 {
		t.Fatalf("unsafe Go version output: stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}
