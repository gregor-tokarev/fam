package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/apple/pkl-go/pkl"

	"github.com/gregor-tokarev/fam/internal/pklgen/base"
	"github.com/gregor-tokarev/fam/internal/pklgen/bundle"
	"github.com/gregor-tokarev/fam/internal/pklgen/bundle/verifyevent"
	pklconfig "github.com/gregor-tokarev/fam/internal/pklgen/config"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks/manager"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks/state"
	"github.com/gregor-tokarev/fam/internal/version"
)

const (
	planCacheDirName   = "plan-cache-v1"
	planCacheMaxFiles  = 128
	planCacheMaxFileSz = 2 << 20
)

// gobNil represents a nil value inside map/slice fields of pkl.Object, since encoding/gob cannot
// encode bare nil interface elements directly.
type gobNil struct{}

type pklObjectDTO struct {
	ModuleUri  string
	Name       string
	Properties map[string]any
	Entries    []pklEntryDTO
	Elements   []any
}

type pklEntryDTO struct {
	Key   any
	Value any
}

type packageRefDTO struct {
	Package string
	Manager string
	Names   map[string]string
}

type taskDTO struct {
	Kind string // file, symlink, merge, package, script, service, linger, skill, github_release

	// Shared fields
	Path     string
	State    string
	Sudo     bool
	Restarts []string

	// File
	Content *string
	Mode    string

	// Symlink
	Target string

	// Merge
	Format string
	Values pklObjectDTO

	// Package
	HasPkg         bool
	PkgVal         any // string, packageRefDTO, or pklObjectDTO
	Binary         *string
	Install        *string
	Update         *string
	Uninstall      *string
	VersionCommand *string

	// Script
	Name    string
	Check   *string
	Creates *string
	Apply   string

	// Service
	Scope   string
	Enabled bool
	Running bool

	// Linger
	User *string

	// AgentSkill
	Source    string
	SkillPath *string
	Ref       *string
	SkillName *string

	// GitHubRelease
	Repo   string
	Assets map[string]tasks.ReleaseAsset
	Tag    *string
}

type verifyDTO struct {
	Name   *string
	Event  string
	Script string
}

type bundleUseDTO struct {
	Name              string
	Supported         bool
	UnsupportedReason *string
	Version           *string
	Source            string
	Manager           *string
	Env               map[string]string
	RequiredSecrets   []string
	Path              []string
	Tasks             []taskDTO
	Verify            []verifyDTO
	Uninstall         []taskDTO
	State             string
	Origin            string
	Dir               string
}

type agentSpecDTO struct {
	Name     string
	Settings pklObjectDTO
	Version  *string
}

type pklDurationDTO struct {
	Value float64
	Unit  int64
}

type pklDataSizeDTO struct {
	Value float64
	Unit  int64
}

type infisicalDTO struct {
	SiteUrl        string
	ProjectId      string
	Environment    string
	Path           string
	Recursive      bool
	IncludeImports bool
	Credentials    *string
	Cache          pklDurationDTO
}

type secretsDTO struct {
	Delivery  string
	File      *string
	Infisical *infisicalDTO
}

type specDTO struct {
	Machine Machine
	Bundles []bundleUseDTO
	Agents  []agentSpecDTO
	Secrets *secretsDTO
}

func init() {
	gob.Register(gobNil{})
	gob.Register(pklObjectDTO{})
	gob.Register(packageRefDTO{})
	gob.Register(pklDurationDTO{})
	gob.Register(pklDataSizeDTO{})
	gob.Register(pkl.IntSeq{})
	gob.Register(pkl.Regex{})
	gob.Register(map[string]any{})
	gob.Register([]any{})
	gob.Register([]string{})
	gob.Register([]int{})
	gob.Register([]int64{})
	gob.Register([]float64{})
	gob.Register([]bool{})
}

func encodeSecrets(s *base.Secrets) *secretsDTO {
	if s == nil {
		return nil
	}
	d := &secretsDTO{Delivery: s.Delivery, File: s.File}
	if s.Infisical != nil {
		d.Infisical = &infisicalDTO{
			SiteUrl:        s.Infisical.SiteUrl,
			ProjectId:      s.Infisical.ProjectId,
			Environment:    s.Infisical.Environment,
			Path:           s.Infisical.Path,
			Recursive:      s.Infisical.Recursive,
			IncludeImports: s.Infisical.IncludeImports,
			Credentials:    s.Infisical.Credentials,
			Cache:          pklDurationDTO{Value: s.Infisical.Cache.Value, Unit: int64(s.Infisical.Cache.Unit)},
		}
	}
	return d
}

func decodeSecrets(d *secretsDTO) *base.Secrets {
	if d == nil {
		return nil
	}
	s := &base.Secrets{Delivery: d.Delivery, File: d.File}
	if d.Infisical != nil {
		s.Infisical = &base.Infisical{
			SiteUrl:        d.Infisical.SiteUrl,
			ProjectId:      d.Infisical.ProjectId,
			Environment:    d.Infisical.Environment,
			Path:           d.Infisical.Path,
			Recursive:      d.Infisical.Recursive,
			IncludeImports: d.Infisical.IncludeImports,
			Credentials:    d.Infisical.Credentials,
			Cache:          pkl.Duration{Value: d.Infisical.Cache.Value, Unit: pkl.DurationUnit(d.Infisical.Cache.Unit)},
		}
	}
	return s
}

func encodeDynamic(v any) any {
	if v == nil {
		return gobNil{}
	}
	switch x := v.(type) {
	case pkl.Object:
		return encodeObject(x)
	case *pkl.Object:
		if x == nil {
			return gobNil{}
		}
		return encodeObject(*x)
	case pkl.Duration:
		return pklDurationDTO{Value: x.Value, Unit: int64(x.Unit)}
	case *pkl.Duration:
		if x == nil {
			return gobNil{}
		}
		return pklDurationDTO{Value: x.Value, Unit: int64(x.Unit)}
	case pkl.DataSize:
		return pklDataSizeDTO{Value: x.Value, Unit: int64(x.Unit)}
	case *pkl.DataSize:
		if x == nil {
			return gobNil{}
		}
		return pklDataSizeDTO{Value: x.Value, Unit: int64(x.Unit)}
	case tasks.PackageRef:
		return encodePkgRef(x)
	case *tasks.PackageRef:
		if x == nil {
			return gobNil{}
		}
		return encodePkgRef(*x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = encodeDynamic(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = encodeDynamic(e)
		}
		return out
	default:
		return v
	}
}

func decodeDynamic(v any) any {
	switch x := v.(type) {
	case gobNil:
		return nil
	case pklObjectDTO:
		return decodeObject(x)
	case pklDurationDTO:
		return pkl.Duration{Value: x.Value, Unit: pkl.DurationUnit(x.Unit)}
	case pklDataSizeDTO:
		return pkl.DataSize{Value: x.Value, Unit: pkl.DataSizeUnit(x.Unit)}
	case packageRefDTO:
		return decodePkgRef(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = decodeDynamic(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = decodeDynamic(e)
		}
		return out
	default:
		return v
	}
}

func encodeObject(o pkl.Object) pklObjectDTO {
	dto := pklObjectDTO{ModuleUri: o.ModuleUri, Name: o.Name}
	if len(o.Properties) > 0 {
		dto.Properties = make(map[string]any, len(o.Properties))
		for k, v := range o.Properties {
			dto.Properties[k] = encodeDynamic(v)
		}
	}
	if len(o.Entries) > 0 {
		dto.Entries = make([]pklEntryDTO, 0, len(o.Entries))
		for k, v := range o.Entries {
			dto.Entries = append(dto.Entries, pklEntryDTO{Key: encodeDynamic(k), Value: encodeDynamic(v)})
		}
	}
	if len(o.Elements) > 0 {
		dto.Elements = make([]any, len(o.Elements))
		for i, e := range o.Elements {
			dto.Elements[i] = encodeDynamic(e)
		}
	}
	return dto
}

func decodeObject(d pklObjectDTO) pkl.Object {
	o := pkl.Object{ModuleUri: d.ModuleUri, Name: d.Name}
	if len(d.Properties) > 0 {
		o.Properties = make(map[string]any, len(d.Properties))
		for k, v := range d.Properties {
			o.Properties[k] = decodeDynamic(v)
		}
	}
	if len(d.Entries) > 0 {
		o.Entries = make(map[any]any, len(d.Entries))
		for _, e := range d.Entries {
			o.Entries[decodeDynamic(e.Key)] = decodeDynamic(e.Value)
		}
	}
	if len(d.Elements) > 0 {
		o.Elements = make([]any, len(d.Elements))
		for i, e := range d.Elements {
			o.Elements[i] = decodeDynamic(e)
		}
	}
	return o
}

func encodePkgRef(r tasks.PackageRef) packageRefDTO {
	d := packageRefDTO{Package: r.Package, Manager: string(r.Manager)}
	if len(r.Names) > 0 {
		d.Names = make(map[string]string, len(r.Names))
		for k, v := range r.Names {
			d.Names[string(k)] = v
		}
	}
	return d
}

func decodePkgRef(d packageRefDTO) tasks.PackageRef {
	r := tasks.PackageRef{Package: d.Package, Manager: manager.Manager(d.Manager)}
	if len(d.Names) > 0 {
		r.Names = make(map[manager.Manager]string, len(d.Names))
		for k, v := range d.Names {
			r.Names[manager.Manager(k)] = v
		}
	}
	return r
}

func encodeTasks(in []tasks.Task) ([]taskDTO, bool) {
	if len(in) == 0 {
		return nil, true
	}
	out := make([]taskDTO, len(in))
	for i, t := range in {
		switch x := t.(type) {
		case tasks.FileImpl:
			out[i] = taskDTO{Kind: "file", Path: x.Path, Content: x.Content, Mode: x.Mode, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
		case *tasks.FileImpl:
			out[i] = taskDTO{Kind: "file", Path: x.Path, Content: x.Content, Mode: x.Mode, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
		case tasks.SymlinkImpl:
			out[i] = taskDTO{Kind: "symlink", Path: x.Path, Target: x.Target, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
		case *tasks.SymlinkImpl:
			out[i] = taskDTO{Kind: "symlink", Path: x.Path, Target: x.Target, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
		case tasks.MergeImpl:
			out[i] = taskDTO{Kind: "merge", Path: x.Path, Format: x.Format, Values: encodeObject(x.Values), Mode: x.Mode, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
		case *tasks.MergeImpl:
			out[i] = taskDTO{Kind: "merge", Path: x.Path, Format: x.Format, Values: encodeObject(x.Values), Mode: x.Mode, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
		case tasks.PackageImpl:
			d := taskDTO{Kind: "package", Binary: x.Binary, Install: x.Install, Update: x.Update, Uninstall: x.Uninstall, VersionCommand: x.VersionCommand, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
			if x.Package != nil {
				d.HasPkg = true
				d.PkgVal = encodeDynamic(*x.Package)
			}
			out[i] = d
		case *tasks.PackageImpl:
			d := taskDTO{Kind: "package", Binary: x.Binary, Install: x.Install, Update: x.Update, Uninstall: x.Uninstall, VersionCommand: x.VersionCommand, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
			if x.Package != nil {
				d.HasPkg = true
				d.PkgVal = encodeDynamic(*x.Package)
			}
			out[i] = d
		case tasks.ScriptImpl:
			out[i] = taskDTO{Kind: "script", Name: x.Name, Check: x.Check, Creates: x.Creates, Apply: x.Apply, Sudo: x.Sudo, Restarts: x.Restarts}
		case *tasks.ScriptImpl:
			out[i] = taskDTO{Kind: "script", Name: x.Name, Check: x.Check, Creates: x.Creates, Apply: x.Apply, Sudo: x.Sudo, Restarts: x.Restarts}
		case tasks.ServiceImpl:
			out[i] = taskDTO{Kind: "service", Name: x.Name, Scope: x.Scope, Content: x.Content, Enabled: x.Enabled, Running: x.Running, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
		case *tasks.ServiceImpl:
			out[i] = taskDTO{Kind: "service", Name: x.Name, Scope: x.Scope, Content: x.Content, Enabled: x.Enabled, Running: x.Running, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
		case tasks.LingerImpl:
			out[i] = taskDTO{Kind: "linger", Sudo: x.Sudo, User: x.User, State: string(x.State), Restarts: x.Restarts}
		case *tasks.LingerImpl:
			out[i] = taskDTO{Kind: "linger", Sudo: x.Sudo, User: x.User, State: string(x.State), Restarts: x.Restarts}
		case tasks.AgentSkillImpl:
			out[i] = taskDTO{Kind: "skill", Source: x.Source, SkillPath: x.Path, Ref: x.Ref, SkillName: x.Name, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
		case *tasks.AgentSkillImpl:
			out[i] = taskDTO{Kind: "skill", Source: x.Source, SkillPath: x.Path, Ref: x.Ref, SkillName: x.Name, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
		case tasks.GitHubReleaseImpl:
			bin := x.Binary
			out[i] = taskDTO{Kind: "github_release", Repo: x.Repo, Binary: &bin, VersionCommand: x.VersionCommand, Assets: x.Assets, Tag: x.Tag, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
		case *tasks.GitHubReleaseImpl:
			bin := x.Binary
			out[i] = taskDTO{Kind: "github_release", Repo: x.Repo, Binary: &bin, VersionCommand: x.VersionCommand, Assets: x.Assets, Tag: x.Tag, State: string(x.State), Sudo: x.Sudo, Restarts: x.Restarts}
		default:
			return nil, false
		}
	}
	return out, true
}

func decodeTasks(in []taskDTO) ([]tasks.Task, bool) {
	if len(in) == 0 {
		return nil, true
	}
	out := make([]tasks.Task, len(in))
	for i, d := range in {
		switch d.Kind {
		case "file":
			out[i] = tasks.FileImpl{Path: d.Path, Content: d.Content, Mode: d.Mode, State: state.State(d.State), Sudo: d.Sudo, Restarts: d.Restarts}
		case "symlink":
			out[i] = tasks.SymlinkImpl{Path: d.Path, Target: d.Target, State: state.State(d.State), Sudo: d.Sudo, Restarts: d.Restarts}
		case "merge":
			out[i] = tasks.MergeImpl{Path: d.Path, Format: d.Format, Values: decodeObject(d.Values), Mode: d.Mode, State: state.State(d.State), Sudo: d.Sudo, Restarts: d.Restarts}
		case "package":
			p := tasks.PackageImpl{Binary: d.Binary, Install: d.Install, Update: d.Update, Uninstall: d.Uninstall, VersionCommand: d.VersionCommand, State: state.State(d.State), Sudo: d.Sudo, Restarts: d.Restarts}
			if d.HasPkg {
				v := decodeDynamic(d.PkgVal)
				p.Package = &v
			}
			out[i] = p
		case "script":
			out[i] = tasks.ScriptImpl{Name: d.Name, Check: d.Check, Creates: d.Creates, Apply: d.Apply, Sudo: d.Sudo, Restarts: d.Restarts}
		case "service":
			out[i] = tasks.ServiceImpl{Name: d.Name, Scope: d.Scope, Content: d.Content, Enabled: d.Enabled, Running: d.Running, State: state.State(d.State), Sudo: d.Sudo, Restarts: d.Restarts}
		case "linger":
			out[i] = tasks.LingerImpl{Sudo: d.Sudo, User: d.User, State: state.State(d.State), Restarts: d.Restarts}
		case "skill":
			out[i] = tasks.AgentSkillImpl{Source: d.Source, Path: d.SkillPath, Ref: d.Ref, Name: d.SkillName, State: state.State(d.State), Sudo: d.Sudo, Restarts: d.Restarts}
		case "github_release":
			bin := ""
			if d.Binary != nil {
				bin = *d.Binary
			}
			out[i] = tasks.GitHubReleaseImpl{Repo: d.Repo, Binary: bin, VersionCommand: d.VersionCommand, Assets: d.Assets, Tag: d.Tag, State: state.State(d.State), Sudo: d.Sudo, Restarts: d.Restarts}
		default:
			return nil, false
		}
	}
	return out, true
}

func specToDTO(s *Spec) (specDTO, bool) {
	dto := specDTO{
		Machine: s.Machine,
		Secrets: encodeSecrets(s.Secrets),
		Bundles: make([]bundleUseDTO, len(s.Bundles)),
		Agents:  make([]agentSpecDTO, len(s.Agents)),
	}
	for i, bu := range s.Bundles {
		ts, ok := encodeTasks(bu.Bundle.Tasks)
		if !ok {
			return specDTO{}, false
		}
		us, ok := encodeTasks(bu.Bundle.Uninstall)
		if !ok {
			return specDTO{}, false
		}
		var mgr *string
		if bu.Bundle.Manager != nil {
			m := string(*bu.Bundle.Manager)
			mgr = &m
		}
		vs := make([]verifyDTO, len(bu.Bundle.Verify))
		for j, v := range bu.Bundle.Verify {
			vs[j] = verifyDTO{Name: v.Name, Event: string(v.Event), Script: v.Script}
		}
		dto.Bundles[i] = bundleUseDTO{
			Name:              bu.Bundle.Name,
			Supported:         bu.Bundle.Supported,
			UnsupportedReason: bu.Bundle.UnsupportedReason,
			Version:           bu.Bundle.Version,
			Source:            bu.Bundle.Source,
			Manager:           mgr,
			Env:               bu.Bundle.Env,
			RequiredSecrets:   bu.Bundle.RequiredSecrets,
			Path:              bu.Bundle.Path,
			Tasks:             ts,
			Verify:            vs,
			Uninstall:         us,
			State:             bu.State,
			Origin:            bu.Origin,
			Dir:               bu.Dir,
		}
	}
	for i, a := range s.Agents {
		dto.Agents[i] = agentSpecDTO{
			Name:     a.Name,
			Settings: encodeObject(a.Agent.Settings),
			Version:  a.Agent.Version,
		}
	}
	return dto, true
}

func dtoToSpec(dto specDTO) (*Spec, bool) {
	s := &Spec{
		Machine: dto.Machine,
		Secrets: decodeSecrets(dto.Secrets),
		Bundles: make([]BundleUse, len(dto.Bundles)),
		Agents:  make([]AgentSpec, len(dto.Agents)),
	}
	for i, b := range dto.Bundles {
		ts, ok := decodeTasks(b.Tasks)
		if !ok {
			return nil, false
		}
		us, ok := decodeTasks(b.Uninstall)
		if !ok {
			return nil, false
		}
		var mgr *manager.Manager
		if b.Manager != nil {
			m := manager.Manager(*b.Manager)
			mgr = &m
		}
		vs := make([]bundle.Verify, len(b.Verify))
		for j, v := range b.Verify {
			vs[j] = bundle.Verify{Name: v.Name, Event: verifyevent.VerifyEvent(v.Event), Script: v.Script}
		}
		s.Bundles[i] = BundleUse{
			Bundle: bundle.Bundle{
				Name:              b.Name,
				Supported:         b.Supported,
				UnsupportedReason: b.UnsupportedReason,
				Version:           b.Version,
				Source:            b.Source,
				Manager:           mgr,
				Env:               b.Env,
				RequiredSecrets:   b.RequiredSecrets,
				Path:              b.Path,
				Tasks:             ts,
				Verify:            vs,
				Uninstall:         us,
			},
			State:  b.State,
			Origin: b.Origin,
			Dir:    b.Dir,
		}
	}
	for i, a := range dto.Agents {
		s.Agents[i] = AgentSpec{
			Name: a.Name,
			Agent: base.Agent{
				Settings: decodeObject(a.Settings),
				Version:  a.Version,
			},
		}
	}
	return s, true
}

// PlanCache caches evaluated Pkl init (machines + secrets) and resolved machine Specs on disk
// keyed by a full content hash of the project files, FAM version, schema checksum, and properties.
type PlanCache struct {
	dir        string
	projectKey string
	readonly   bool
	disabled   bool
}

type cachedInit struct {
	Machines []Machine
	Secrets  *secretsDTO
}

func planCacheDir() string {
	if x := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "fam", planCacheDirName)
	}
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "fam", planCacheDirName)
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".cache", "fam", planCacheDirName)
	}
	return ""
}

// NewPlanCache fingerprints base and configPath and returns a disk-backed PlanCache.
// If any file references external URLs, parent/absolute paths outside base, or symlinks outside
// base, caching is automatically disabled to guarantee zero stale hits.
func NewPlanCache(baseDir, configPath, pkgChecksum string, readonly bool) *PlanCache {
	c := &PlanCache{dir: planCacheDir(), readonly: readonly}
	if c.dir == "" || os.Getenv("FAM_NO_PLAN_CACHE") == "1" {
		c.disabled = true
		return c
	}
	key, ok := fingerprintProject(baseDir, configPath, pkgChecksum, readonly)
	if !ok {
		c.disabled = true
		return c
	}
	c.projectKey = key
	return c
}

func fingerprintProject(baseDir, configPath, pkgChecksum string, readonly bool) (string, bool) {
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", false
	}
	realBase, err := filepath.EvalSymlinks(absBase)
	if err != nil {
		realBase = absBase
	}
	type fileEntry struct {
		rel  string
		path string
	}
	var entries []fileEntry
	walkErr := filepath.WalkDir(absBase, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != absBase && (name == ".git" || name == ".cache" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(p)
			if err != nil {
				return fs.ErrInvalid
			}
			rel, err := filepath.Rel(realBase, target)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fs.ErrInvalid
			}
			info, err := os.Stat(target)
			if err != nil || info.IsDir() {
				return fs.ErrInvalid
			}
		} else if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(absBase, p)
		if err != nil {
			return err
		}
		entries = append(entries, fileEntry{rel: filepath.ToSlash(rel), path: p})
		return nil
	})
	if walkErr != nil {
		return "", false
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	h := sha256.New()
	fmt.Fprintf(h, "v2\x00%s\x00%s\x00%s\x00%s\x00%t\x00", version.Version, pkgChecksum, absBase, configPath, readonly)
	readsEnv := false
	for _, e := range entries {
		info, err := os.Stat(e.path)
		if err != nil || info.Size() > planCacheMaxFileSz {
			return "", false
		}
		b, err := os.ReadFile(e.path)
		if err != nil {
			return "", false
		}
		if strings.HasSuffix(e.rel, ".pkl") || e.rel == "PklProject" {
			if hasExternalPklRef(b) {
				return "", false
			}
			if bytes.Contains(b, []byte("env:")) {
				readsEnv = true
			}
		}
		fsum := sha256.Sum256(b)
		h.Write([]byte(e.rel))
		h.Write([]byte{0})
		h.Write(fsum[:])
		h.Write([]byte{0})
	}
	if readsEnv {
		env := append([]string{}, os.Environ()...)
		sort.Strings(env)
		h.Write([]byte("env\x00"))
		for _, kv := range env {
			h.Write([]byte(kv))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func hasExternalPklRef(b []byte) bool {
	for _, pat := range [][]byte{
		[]byte(`read("http:`), []byte(`read("https:`),
		[]byte(`import("http:`), []byte(`import("https:`),
		[]byte(`read("/`), []byte(`import("/`), []byte(`amends "/`),
		[]byte(`read("file:/`), []byte(`import("file:/`),
		[]byte(`read("..`), []byte(`import("..`), []byte(`amends "..`),
		[]byte(`read?("http:`), []byte(`read?("https:`), []byte(`read?("/`), []byte(`read?("..`),
		[]byte(`read*(`), []byte(`import*(`),
	} {
		if bytes.Contains(b, pat) {
			return true
		}
	}
	return false
}

func (c *PlanCache) entryPath(kind, extra string, props map[string]string) string {
	h := sha256.New()
	h.Write([]byte(c.projectKey))
	h.Write([]byte{0})
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(extra))
	h.Write([]byte{0})
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{'='})
		h.Write([]byte(props[k]))
		h.Write([]byte{0})
	}
	return filepath.Join(c.dir, hex.EncodeToString(h.Sum(nil))+".gob")
}

func (c *PlanCache) GetInit(props map[string]string) ([]Machine, *base.Secrets, bool) {
	if c == nil || c.disabled {
		return nil, nil, false
	}
	b, err := os.ReadFile(c.entryPath("init", "", props))
	if err != nil {
		return nil, nil, false
	}
	var ci cachedInit
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&ci); err != nil {
		return nil, nil, false
	}
	return ci.Machines, decodeSecrets(ci.Secrets), true
}

func (c *PlanCache) PutInit(props map[string]string, machines []Machine, sec *base.Secrets) {
	if c == nil || c.disabled || c.readonly {
		return
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(cachedInit{Machines: machines, Secrets: encodeSecrets(sec)}); err != nil {
		return
	}
	c.writeEntry(c.entryPath("init", "", props), buf.Bytes())
}

func (c *PlanCache) GetSpec(machine string, props map[string]string) (*Spec, bool) {
	if c == nil || c.disabled {
		return nil, false
	}
	b, err := os.ReadFile(c.entryPath("spec", machine, props))
	if err != nil {
		return nil, false
	}
	var dto specDTO
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&dto); err != nil {
		return nil, false
	}
	return dtoToSpec(dto)
}

func (c *PlanCache) PutSpec(machine string, props map[string]string, spec *Spec) {
	if c == nil || c.disabled || c.readonly || spec == nil {
		return
	}
	dto, ok := specToDTO(spec)
	if !ok {
		return
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(dto); err != nil {
		return
	}
	c.writeEntry(c.entryPath("spec", machine, props), buf.Bytes())
}

func (c *PlanCache) writeEntry(dst string, data []byte) {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	_ = os.Chmod(tmpName, 0o600)
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return
	}
	c.prune()
}

func (c *PlanCache) prune() {
	entries, err := os.ReadDir(c.dir)
	if err != nil || len(entries) <= planCacheMaxFiles {
		return
	}
	type item struct {
		path string
		mod  int64
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".gob") {
			continue
		}
		if info, err := e.Info(); err == nil {
			items = append(items, item{path: filepath.Join(c.dir, e.Name()), mod: info.ModTime().UnixNano()})
		}
	}
	if len(items) <= planCacheMaxFiles {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod < items[j].mod })
	for i := 0; i < len(items)-planCacheMaxFiles; i++ {
		_ = os.Remove(items[i].path)
	}
}

// ConfigError wraps a non-Pkl config resolution/validation error while preserving its exact message.
type ConfigError struct {
	Err error
}

func (e *ConfigError) Error() string { return e.Err.Error() }
func (e *ConfigError) Unwrap() error { return e.Err }

// LazyEvaluator wraps PlanCache and only starts the external Pkl process on a cache miss.
type LazyEvaluator struct {
	base       string
	configPath string
	progress   io.Writer
	cache      *PlanCache

	mu sync.Mutex
	ev *Evaluator
}

// NewLazyEvaluator creates a cached evaluator that avoids spawning Pkl when cached results match.
func NewLazyEvaluator(baseDir, configPath, pkgChecksum string, progress io.Writer) *LazyEvaluator {
	return &LazyEvaluator{
		base:       baseDir,
		configPath: configPath,
		progress:   progress,
		cache:      NewPlanCache(baseDir, configPath, pkgChecksum, false),
	}
}

func (l *LazyEvaluator) get(ctx context.Context) (*Evaluator, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ev != nil {
		return l.ev, nil
	}
	ev, err := NewEvaluator(ctx, l.base, l.configPath, l.progress)
	if err != nil {
		return nil, err
	}
	l.ev = ev
	return ev, nil
}

// Init returns cached (machines, secrets) or evaluates with Pkl on miss.
func (l *LazyEvaluator) Init(ctx context.Context, props map[string]string) ([]Machine, *base.Secrets, error) {
	if ms, sec, ok := l.cache.GetInit(props); ok {
		return ms, sec, nil
	}
	ev, err := l.get(ctx)
	if err != nil {
		return nil, nil, err
	}
	ms, sec, err := ev.Init(ctx, props)
	if err != nil {
		return nil, nil, err
	}
	l.cache.PutInit(props, ms, sec)
	return ms, sec, nil
}

// EvalSpec returns a cached validated+resolved Spec for machine, or evaluates with Pkl on miss.
func (l *LazyEvaluator) EvalSpec(ctx context.Context, machine string, props map[string]string) (*Spec, error) {
	if spec, ok := l.cache.GetSpec(machine, props); ok {
		return spec, nil
	}
	ev, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := ev.Eval(ctx, props)
	if err != nil {
		return nil, err
	}
	spec, err := Resolve(cfg, machine)
	if err != nil {
		return nil, &ConfigError{Err: err}
	}
	l.cache.PutSpec(machine, props, spec)
	return spec, nil
}

// Eval evaluates the raw Config via Pkl.
func (l *LazyEvaluator) Eval(ctx context.Context, props map[string]string) (*pklconfig.Config, error) {
	ev, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return ev.Eval(ctx, props)
}

// Close closes the underlying Pkl evaluator if it was started.
func (l *LazyEvaluator) Close() error {
	l.mu.Lock()
	ev := l.ev
	l.ev = nil
	l.mu.Unlock()
	if ev != nil {
		return ev.Close()
	}
	return nil
}
