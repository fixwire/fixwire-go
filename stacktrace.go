package fixwire

import (
	"bufio"
	"io"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// The SDK's own packages: their frames are left out of stacks.
const sdkModule = "github.com/fixwire/fixwire-go"

// stackSlack is room for the frames read and then left out: the SDK's own,
// and those of a recover above runtime.gopanic.
const stackSlack = 32

// stack is the calling goroutine's stack, the oldest call first, without
// the SDK's own frames.
func stack(opts Options) []Frame {
	pcs := make([]uintptr, opts.MaxStackFrames+stackSlack)
	n := runtime.Callers(1, pcs)
	return framesOf(pcs[:n], opts, false)
}

// panicStack is the stack of a panicking goroutine from inside its deferred
// recover: the frames below runtime.gopanic.
func panicStack(opts Options) []Frame {
	pcs := make([]uintptr, opts.MaxStackFrames+stackSlack)
	n := runtime.Callers(1, pcs)
	return framesOf(pcs[:n], opts, true)
}

// errorStack is the stack an error recorded where it was made: a
// StackTrace() or Callers() method returning program counters (pkg/errors,
// go-errors and others); nil for other errors.
func errorStack(err error, opts Options) []Frame {
	v := reflect.ValueOf(err)
	for _, name := range []string{"StackTrace", "Callers"} {
		m := v.MethodByName(name)
		if !m.IsValid() || m.Type().NumIn() != 0 || m.Type().NumOut() != 1 {
			continue
		}
		t := m.Type().Out(0)
		if t.Kind() != reflect.Slice || t.Elem().Kind() != reflect.Uintptr {
			continue
		}
		pcs := m.Call(nil)[0]
		out := make([]uintptr, min(pcs.Len(), opts.MaxStackFrames+stackSlack)) // the newest
		for i := range out {
			out[i] = uintptr(pcs.Index(i).Uint())
		}
		return framesOf(out, opts, false)
	}
	return nil
}

// framesOf reads program counters (newest first) into frames, oldest
// first, the newest MaxStackFrames of them. fromPanic keeps only what was
// below runtime.gopanic.
func framesOf(pcs []uintptr, opts Options, fromPanic bool) []Frame {
	iter := runtime.CallersFrames(pcs)
	var newest []Frame
	panicked := !fromPanic
	for {
		f, more := iter.Next()
		switch {
		case f.Function == "runtime.gopanic":
			panicked, newest = true, newest[:0]
		case !panicked || isSDK(f.Function) || f.Function == "runtime.goexit" || f.Function == "":
		default:
			module, function := splitFunction(f.Function)
			newest = append(newest, Frame{
				Function: function, Module: module, File: shortFile(f.File, module), AbsPath: f.File, Line: f.Line,
				InApp: inApp(module, f.File, opts),
			})
		}
		if !more {
			break
		}
	}
	newest = newest[:min(len(newest), opts.MaxStackFrames)]
	frames := make([]Frame, len(newest))
	for i, f := range newest {
		frames[len(newest)-1-i] = f
	}
	if opts.ContextLines > 0 {
		for i := range frames {
			if frames[i].InApp {
				addContext(&frames[i], opts.ContextLines)
			}
		}
	}
	return frames
}

func isSDK(function string) bool {
	return strings.HasPrefix(function, sdkModule+".") || strings.HasPrefix(function, sdkModule+"/")
}

// splitFunction splits "github.com/acme/shop/cart.(*Cart).Charge" into its
// package path and its function.
func splitFunction(name string) (module, function string) {
	slash := strings.LastIndexByte(name, '/')
	if dot := strings.IndexByte(name[slash+1:], '.'); dot >= 0 {
		return name[:slash+1+dot], name[slash+1+dot+1:]
	}
	return "", name
}

// shortFile is a frame's file relative to its package's directory, when
// the path shows it ("cart/cart.go").
func shortFile(file, module string) string {
	if module != "" {
		last := module[strings.LastIndexByte(module, '/')+1:]
		if i := strings.LastIndex(file, "/"+last+"/"); i >= 0 {
			return file[i+1:]
		}
	}
	return file
}

// inApp reports whether a frame is the app's: the main module's packages
// are, its dependencies and the standard library are not, unless the
// options say otherwise.
func inApp(module, file string, opts Options) bool {
	for _, p := range opts.InAppExclude {
		if strings.HasPrefix(module, p) {
			return false
		}
	}
	for _, p := range opts.InAppInclude {
		if strings.HasPrefix(module, p) {
			return true
		}
	}
	b := build()
	switch {
	case module == "main", b.main != "" && within(module, b.main):
		return true
	case module == "":
		return false
	}
	// A dependency's package: the module or one of its parents is one.
	for p := module; ; {
		if b.deps[p] {
			return false
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			break
		}
		p = p[:i]
	}
	first, _, _ := strings.Cut(module, "/")
	if !strings.Contains(first, ".") {
		return false // the standard library
	}
	return !strings.Contains(file, "/pkg/mod/") && !strings.Contains(file, "/vendor/")
}

// within reports whether a package is the module or one of its packages.
func within(pkg, module string) bool {
	return pkg == module || strings.HasPrefix(pkg, module+"/")
}

// buildInfo is the binary's main module and dependencies.
type buildInfo struct {
	main string
	deps map[string]bool
}

var build = sync.OnceValue(func() buildInfo {
	b := buildInfo{deps: map[string]bool{}}
	if info, ok := debug.ReadBuildInfo(); ok {
		b.main = info.Main.Path
		for _, d := range info.Deps {
			b.deps[d.Path] = true
		}
	}
	return b
})

// source caches the lines of the files frames point to.
var source = struct {
	sync.Mutex
	files map[string][]string
	bytes int // of the files cached
}{files: map[string][]string{}}

// maxSourceFiles and maxSourceCache bound the cache; maxSourceBytes skips
// larger files.
const (
	maxSourceFiles = 64
	maxSourceCache = 32 << 20
	maxSourceBytes = 10 << 20
)

func addContext(f *Frame, n int) {
	lines := sourceLines(f.AbsPath)
	i := f.Line - 1
	if i < 0 || i >= len(lines) {
		return
	}
	f.ContextLine = lines[i]
	f.PreContext = lines[max(0, i-n):i]
	f.PostContext = lines[i+1 : min(len(lines), i+1+n)]
}

func sourceLines(path string) []string {
	source.Lock()
	lines, ok := source.files[path]
	source.Unlock()
	if ok {
		return lines
	}
	// Read without the lock, so that a slow disk holds up this capture
	// only; regular files only (a pipe would block).
	size := 0
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Size() <= maxSourceBytes {
		if f, err := os.Open(path); err == nil {
			sc := bufio.NewScanner(io.LimitReader(f, maxSourceBytes))
			for sc.Scan() {
				lines = append(lines, sc.Text())
				size += len(sc.Text())
			}
			_ = f.Close()
		}
	}
	source.Lock()
	defer source.Unlock()
	if _, ok := source.files[path]; ok {
		return lines // another capture read it meanwhile
	}
	for k, cached := range source.files {
		if len(source.files) < maxSourceFiles && source.bytes+size <= maxSourceCache {
			break
		}
		delete(source.files, k)
		source.bytes -= linesSize(cached)
	}
	source.files[path] = lines
	source.bytes += size
	return lines
}

func linesSize(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l)
	}
	return n
}
