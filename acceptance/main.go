// Command acceptance is the VM acceptance harness of FAM.
//
//	go run ./acceptance prepare <target>...   build the provisioned base images
//	go run ./acceptance up <target>           boot a clean VM and wait (Ctrl-C stops it)
//	go run ./acceptance run [-scenarios 1,2] <target>...
//	go run ./acceptance windows               Windows in dockur/windows: FAM must stop with a clear error
//	go run ./acceptance clean                 delete base images and runtime files
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
)

var stdGroups = os.Getenv("FAM_STD_GROUPS")

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: acceptance prepare|up|run|clean ...")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	w, err := newWork()
	if err != nil {
		fail(err)
	}
	switch os.Args[1] {
	case "prepare":
		fs := flag.NewFlagSet("prepare", flag.ExitOnError)
		force := fs.Bool("force", false, "rebuild existing base images")
		fs.Parse(os.Args[2:])
		for _, name := range fs.Args() {
			t, err := findTarget(name)
			if err != nil {
				fail(err)
			}
			fmt.Printf("prepare %s\n", t.Name)
			if err := w.Prepare(ctx, t, *force); err != nil {
				fail(err)
			}
			fmt.Printf("prepared %s\n", w.basePath(t))
		}
	case "up":
		t, err := findTarget(os.Args[2])
		if err != nil {
			fail(err)
		}
		vm, err := w.Start(ctx, t)
		if err != nil {
			fail(err)
		}
		fmt.Printf("%s is up: ssh -F %s %s\n", t.Name, w.SSHCfg, alias(t.Name))
		<-ctx.Done()
		vm.Kill()
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		scen := fs.String("scenarios", "", "comma separated scenario numbers (default: all)")
		fs.StringVar(&stdGroups, "std-groups", stdGroups, "comma separated standard groups for scenario 16 (default: all)")
		fam := fs.String("fam", "", "fam binary (default: build it)")
		fs.Parse(os.Args[2:])
		if err := runMatrix(ctx, w, fs.Args(), *scen, *fam); err != nil {
			fail(err)
		}
	case "windows":
		fs := flag.NewFlagSet("windows", flag.ExitOnError)
		keep := fs.Bool("keep", false, "keep the container and its disk")
		fs.Parse(os.Args[2:])
		fam, err := buildFam(ctx)
		if err != nil {
			fail(err)
		}
		if err := runWindows(ctx, w, fam, *keep); err != nil {
			fail(err)
		}
	case "clean":
		_ = os.RemoveAll(w.Bases)
		_ = os.RemoveAll(filepath.Join(w.Dir, "run"))
		fmt.Println("removed base images and runtime files; cloud images stay in", w.Images)
	default:
		fail(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "acceptance:", err)
	os.Exit(1)
}
