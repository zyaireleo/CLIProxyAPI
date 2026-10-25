// Command verify-plugin-artifact statically checks a Linux amd64 plugin candidate.
// It never runs the candidate, the Go toolchain, or a system dependency inspector.
package main

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/version"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
)

const usage = "usage: verify-plugin-artifact [--max-glibc 2.35] BINARY"

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(?:\.[0-9]+)?$`)

type glibcVersion struct {
	parts [3]uint64
	text  string
}

func parseVersion(text string) (glibcVersion, error) {
	v := glibcVersion{text: text}
	if !versionPattern.MatchString(text) {
		return v, errors.New("version must be numeric major.minor or major.minor.patch")
	}
	for i, part := range strings.Split(text, ".") {
		n, errParse := strconv.ParseUint(part, 10, 64)
		if errParse != nil {
			return v, errors.New("version component is out of range")
		}
		v.parts[i] = n
	}
	return v, nil
}

func compareVersions(a, b glibcVersion) int {
	for i := range a.parts {
		if a.parts[i] < b.parts[i] {
			return -1
		}
		if a.parts[i] > b.parts[i] {
			return 1
		}
	}
	return 0
}

type artifactReport struct {
	SHA256           string            `json:"sha256"`
	GoVersion        string            `json:"go_version"`
	Settings         map[string]string `json:"settings"`
	ELFType          string            `json:"elf_type"`
	Interpreter      string            `json:"interpreter"`
	Dependencies     []string          `json:"dependencies"`
	GLIBCVersions    []string          `json:"glibc_versions"`
	MaxRequiredGLIBC string            `json:"max_required_glibc"`
	MaxGLIBC         string            `json:"max_glibc"`
}

func validateBuildInfo(info *debug.BuildInfo) (map[string]string, error) {
	if info == nil || !version.IsValid(info.GoVersion) || strings.Contains(info.GoVersion, "-") {
		return nil, errors.New("missing, invalid, or custom-suffixed Go toolchain version")
	}
	required := []debug.BuildSetting{
		{Key: "CGO_ENABLED", Value: "1"},
		{Key: "GOOS", Value: "linux"},
		{Key: "GOARCH", Value: "amd64"},
		{Key: "GOAMD64", Value: "v1"},
	}
	settings := make(map[string]string)
	for _, setting := range info.Settings {
		switch setting.Key {
		case "CGO_ENABLED", "GOOS", "GOARCH", "GOAMD64", "-buildmode":
			if _, exists := settings[setting.Key]; exists {
				return nil, fmt.Errorf("duplicate build setting %s", setting.Key)
			}
			settings[setting.Key] = setting.Value
		}
	}
	for _, want := range required {
		got, exists := settings[want.Key]
		if !exists {
			// Missing GOAMD64 cannot establish the required baseline either.
			return nil, fmt.Errorf("missing required build setting %s", want.Key)
		}
		if got != want.Value {
			return nil, fmt.Errorf("build setting %s must equal %s", want.Key, want.Value)
		}
	}
	if mode, exists := settings["-buildmode"]; exists && mode != "exe" && mode != "pie" {
		return nil, errors.New("build setting -buildmode must be exe or pie")
	}
	// Do not serialize all of BuildInfo: linker flags, module paths, and other
	// settings can contain credentials or private build-environment details.
	return settings, nil
}

func validateELF(f *elf.File) (string, error) {
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Machine != elf.EM_X86_64 {
		return "", errors.New("ELF must be little-endian 64-bit x86_64")
	}
	if f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX {
		return "", errors.New("ELF ABI must be System V or Linux")
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return "", errors.New("ELF must be an executable or PIE (ET_EXEC or ET_DYN)")
	}
	entryExecutable := false
	var interpreter string
	for _, program := range f.Progs {
		if program.Type == elf.PT_LOAD && program.Flags&elf.PF_X != 0 &&
			f.Entry >= program.Vaddr && f.Entry-program.Vaddr < program.Filesz {
			entryExecutable = true
		}
		if program.Type != elf.PT_INTERP {
			continue
		}
		if interpreter != "" {
			return "", errors.New("ELF must have exactly one PT_INTERP")
		}
		if program.Filesz < 2 || program.Filesz > 4096 {
			return "", errors.New("invalid ELF PT_INTERP size")
		}
		data := make([]byte, int(program.Filesz))
		if _, errRead := io.ReadFull(program.Open(), data); errRead != nil {
			return "", errors.New("cannot read ELF PT_INTERP")
		}
		if data[0] != '/' || data[len(data)-1] != 0 || bytes.IndexByte(data[:len(data)-1], 0) >= 0 {
			return "", errors.New("ELF PT_INTERP must be an absolute NUL-terminated path")
		}
		interpreter = string(data[:len(data)-1])
		if !printableMetadata(interpreter) {
			return "", errors.New("invalid characters in ELF PT_INTERP")
		}
	}
	if f.Entry == 0 || !entryExecutable {
		return "", errors.New("ELF must have a nonzero entry point in an executable PT_LOAD")
	}
	if interpreter == "" {
		return "", errors.New("ELF is missing PT_INTERP; static/no-plugin candidates are not accepted")
	}
	return interpreter, nil
}

func printableMetadata(text string) bool {
	if text == "" {
		return false
	}
	for _, c := range text {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// checkVersionNeeds rejects truncated tables before debug/elf reads them. Its
// version parser can otherwise return partial results for truncated sections.
func checkVersionNeeds(f *elf.File) error {
	section := f.SectionByType(elf.SHT_GNU_VERNEED)
	if section == nil || section.Info == 0 {
		return errors.New("ELF is missing version-need requirements")
	}
	data, errData := section.Data()
	if errData != nil {
		return errors.New("cannot read ELF version-need table")
	}
	if uint64(section.Link) >= uint64(len(f.Sections)) || f.Sections[section.Link].Type != elf.SHT_STRTAB {
		return errors.New("invalid ELF version-need string table")
	}
	symbols := f.SectionByType(elf.SHT_DYNSYM)
	if symbols == nil || symbols.Link != section.Link {
		return errors.New("ELF version-need and dynamic symbols must use the same string table")
	}
	stringsData, errStrings := f.Sections[section.Link].Data()
	if errStrings != nil {
		return errors.New("cannot read ELF version-need string table")
	}
	validString := func(offset uint32) bool {
		return uint64(offset) < uint64(len(stringsData)) &&
			stringsData[offset] != 0 && bytes.IndexByte(stringsData[offset:], 0) >= 0
	}
	badTable := errors.New("malformed or truncated ELF version-need table")
	offset := uint64(0)
	for record := uint32(0); record < section.Info; record++ {
		if offset+16 > uint64(len(data)) {
			return badTable
		}
		header := data[offset : offset+16]
		count := f.ByteOrder.Uint16(header[2:4])
		aux := uint64(f.ByteOrder.Uint32(header[8:12]))
		next := uint64(f.ByteOrder.Uint32(header[12:16]))
		if f.ByteOrder.Uint16(header[:2]) != 1 || count == 0 || aux < 16 ||
			!validString(f.ByteOrder.Uint32(header[4:8])) {
			return badTable
		}
		end := uint64(len(data))
		if record+1 < section.Info {
			if next < 16 || offset+next > end {
				return badTable
			}
			end = offset + next
		} else if next != 0 {
			return badTable
		}
		position := offset + aux
		for index := uint16(0); index < count; index++ {
			if position+16 > end {
				return badTable
			}
			entry := data[position : position+16]
			if !validString(f.ByteOrder.Uint32(entry[8:12])) {
				return badTable
			}
			auxNext := uint64(f.ByteOrder.Uint32(entry[12:16]))
			if index+1 < count {
				if auxNext < 16 {
					return badTable
				}
			} else if auxNext != 0 {
				return badTable
			}
			position += auxNext
		}
		offset += next
	}
	return nil
}

func requiredGLIBC(f *elf.File, maximum glibcVersion) ([]string, error) {
	if errTable := checkVersionNeeds(f); errTable != nil {
		return nil, errTable
	}
	needs, errNeeds := f.DynamicVersionNeeds()
	if errNeeds != nil {
		return nil, errors.New("cannot read ELF dynamic version requirements (symbol/version tables required)")
	}
	seen := make(map[string]bool)
	var versions []glibcVersion
	for _, library := range needs {
		for _, requirement := range library.Needs {
			name := requirement.Dep
			if !strings.HasPrefix(name, "GLIBC_") {
				continue
			}
			v, errVersion := parseVersion(strings.TrimPrefix(name, "GLIBC_"))
			if errVersion != nil {
				// GLIBC_PRIVATE and ABI feature labels cannot be proven compatible
				// with a numeric ceiling; never turn them into partial versions.
				return nil, errors.New("unsupported nonnumeric GLIBC requirement in ELF version-need table")
			}
			if compareVersions(v, maximum) > 0 {
				return nil, fmt.Errorf("required GLIBC_%s exceeds --max-glibc %s", v.text, maximum.text)
			}
			if !seen[v.text] {
				seen[v.text] = true
				versions = append(versions, v)
			}
		}
	}
	if len(versions) == 0 {
		return nil, errors.New("no exact numeric GLIBC requirements found in ELF version-need table")
	}
	sort.Slice(versions, func(i, j int) bool {
		if order := compareVersions(versions[i], versions[j]); order != 0 {
			return order < 0
		}
		return versions[i].text < versions[j].text
	})
	result := make([]string, len(versions))
	for i, v := range versions {
		result[i] = "GLIBC_" + v.text
	}
	return result, nil
}

func verifyArtifact(filename string, maximum glibcVersion) (_ *artifactReport, err error) {
	// Release archives use "no-plugin" for builds without dynamic-plugin
	// support. This guard supplements, not replaces, the CGO and ELF checks;
	// none of these checks proves compatibility with a particular plugin ABI.
	name := strings.ToLower(filepath.Base(filename))
	name = strings.NewReplacer("-", "", "_", "").Replace(name)
	if strings.Contains(name, "noplugin") {
		return nil, errors.New("filename marks a no-plugin artifact; release naming denotes builds without dynamic-plugin support")
	}
	file, errOpen := os.Open(filename)
	if errOpen != nil {
		return nil, errors.New("cannot open candidate binary")
	}
	defer func() {
		if errClose := file.Close(); errClose != nil && err == nil {
			err = errors.New("cannot close candidate binary")
		}
	}()
	stat, errStat := file.Stat()
	if errStat != nil || !stat.Mode().IsRegular() || stat.Size() == 0 {
		return nil, errors.New("candidate must be a nonempty regular binary file")
	}
	image, errELF := elf.NewFile(file)
	if errELF != nil {
		return nil, errors.New("candidate is not a readable ELF binary")
	}
	interpreter, errInterpreter := validateELF(image)
	if errInterpreter != nil {
		return nil, errInterpreter
	}
	info, errInfo := buildinfo.Read(file)
	if errInfo != nil {
		// Parser errors may quote arbitrary build settings, including secrets.
		return nil, errors.New("cannot read Go build information from candidate")
	}
	settings, errSettings := validateBuildInfo(info)
	if errSettings != nil {
		return nil, errSettings
	}
	if settings["-buildmode"] == "pie" && image.Type != elf.ET_DYN {
		return nil, errors.New("Go build setting pie does not match ELF type ET_DYN")
	}
	dependencies, errDependencies := image.ImportedLibraries()
	if errDependencies != nil {
		return nil, errors.New("cannot read ELF DT_NEEDED dynamic library dependencies")
	}
	if len(dependencies) == 0 {
		return nil, errors.New("ELF must have nonempty DT_NEEDED dynamic library dependencies")
	}
	for _, dependency := range dependencies {
		if !printableMetadata(dependency) {
			return nil, errors.New("invalid or empty ELF dynamic library dependency")
		}
	}
	sort.Strings(dependencies)
	glibc, errGLIBC := requiredGLIBC(image, maximum)
	if errGLIBC != nil {
		return nil, errGLIBC
	}
	hash := sha256.New()
	read, errHash := io.Copy(hash, io.NewSectionReader(file, 0, stat.Size()))
	if errHash != nil || read != stat.Size() {
		return nil, errors.New("cannot hash complete candidate binary")
	}
	after, errAfter := file.Stat()
	if errAfter != nil || after.Size() != stat.Size() || !after.ModTime().Equal(stat.ModTime()) {
		return nil, errors.New("candidate changed during verification")
	}
	return &artifactReport{
		SHA256:           hex.EncodeToString(hash.Sum(nil)),
		GoVersion:        info.GoVersion,
		Settings:         settings,
		ELFType:          image.Type.String(),
		Interpreter:      interpreter,
		Dependencies:     dependencies,
		GLIBCVersions:    glibc,
		MaxRequiredGLIBC: glibc[len(glibc)-1],
		MaxGLIBC:         maximum.text,
	}, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("verify-plugin-artifact", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	maxText := flags.String("max-glibc", "2.35", "maximum supported glibc version")
	if errFlags := flags.Parse(args); errFlags != nil {
		fmt.Fprintln(stderr, usage)
		if errors.Is(errFlags, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "invalid command-line options")
		return 2
	}
	if flags.NArg() != 1 || flags.Arg(0) == "" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	maximum, errMaximum := parseVersion(*maxText)
	if errMaximum != nil {
		fmt.Fprintln(stderr, "invalid --max-glibc:", errMaximum)
		return 2
	}
	report, errVerify := verifyArtifact(flags.Arg(0), maximum)
	if errVerify != nil {
		fmt.Fprintln(stderr, "artifact verification failed:", errVerify)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if errEncode := encoder.Encode(report); errEncode != nil {
		fmt.Fprintln(stderr, "cannot write artifact JSON")
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
