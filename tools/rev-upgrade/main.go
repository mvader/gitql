package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	git "gopkg.in/src-d/go-git.v4"
)

const (
	lockFile      = "Gopkg.lock"
	goMysqlServer = "github.com/src-d/go-mysql-server"
)

type project struct {
	Name     string
	Revision string
}

type projects struct {
	Projects []project
}

func init() {
	flag.Usage = func() {
		fmt.Println("\ngo run ./tools/rev-upgrade/main.go [-p \"project name\"] [-r \"revision\"]")
		flag.PrintDefaults()
	}
}

func main() {
	var (
		prj    string
		newRev string
		oldRev string

		w   *git.Worktree
		err error
	)

	flag.StringVar(&prj, "p", goMysqlServer, "project name (e.g.: github.com/src-d/go-mysql-server)")
	flag.StringVar(&newRev, "r", "", "revision (by default the latest allowed by Gopkg.toml)")
	flag.Parse()

	if prj == "" {
		log.Fatalln("Project's name cannot be an empty string")
	}

	w, err = worktree()
	if err != nil {
		log.Fatalln(err)
	}

	oldRev, err = revision(filepath.Join(w.Filesystem.Root(), "Gopkg.lock"), prj)
	if err != nil {
		log.Fatalf("Current revision of %s is an empty string (%s)", prj, err)
	}

	if oldRev == newRev {
		return
	}

	defer func() {
		if err != nil {
			log.Println(err)
			w.Reset(&git.ResetOptions{Mode: git.MixedReset})
		}
	}()

	if newRev != "" {
		fmt.Printf("Project: %s\nOld rev: %s\nNew rev: %s\n", prj, oldRev, newRev)

		if err = replace(w, oldRev, newRev); err != nil {
			return
		}
	}

	if err = ensure(prj); err != nil {
		return
	}

	if prj == goMysqlServer {
		if err = importDocs(); err != nil {
			return
		}
	}

	if newRev == "" {
		newRev, err = revision(filepath.Join(w.Filesystem.Root(), "Gopkg.lock"), prj)
		fmt.Printf("Project: %s\nOld rev: %s\nNew rev: %s\n", prj, oldRev, newRev)
		if newRev == oldRev {
			return
		}

		if err = replace(w, oldRev, newRev); err != nil {
			return
		}
	}
}

// repo's worktree
func worktree() (*git.Worktree, error) {
	repo, err := git.PlainOpenWithOptions(".", &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, err
	}

	return repo.Worktree()
}

// project's current revision
func revision(gopkg string, prj string) (string, error) {
	data, err := ioutil.ReadFile(gopkg)
	if err != nil {
		return "", err
	}
	var projects = projects{}
	if err = toml.Unmarshal(data, &projects); err != nil {
		return "", err
	}
	for _, p := range projects.Projects {
		if p.Name == prj {
			return p.Revision, nil
		}
	}
	return "", io.EOF
}

func replace(w *git.Worktree, oldRev, newRev string) error {
	rexp, err := regexp.Compile(oldRev)
	if err != nil {
		return err
	}

	res, err := w.Grep(&git.GrepOptions{Patterns: []*regexp.Regexp{rexp}})
	if err != nil {
		return err
	}

	files := make(map[string]struct{})
	for _, r := range res {
		// ignore replacements on lockfile so update works
		if r.FileName == lockFile {
			continue
		}

		if _, ok := files[r.FileName]; !ok {
			files[r.FileName] = struct{}{}
		}
	}

	// replace oldRev by newRev in place
	var (
		wg sync.WaitGroup
	)
	for f := range files {
		wg.Add(1)
		go func(filename string, old, new []byte) {
			defer wg.Done()

			d, e := ioutil.ReadFile(filename)
			if e != nil {
				err = e
				return
			}

			d = bytes.Replace(d, old, new, -1)

			e = ioutil.WriteFile(filename, d, 0)
			if e != nil {
				err = e
			}

			fmt.Println("#", filename)
		}(filepath.Join(w.Filesystem.Root(), f), []byte(oldRev), []byte(newRev))
	}
	wg.Wait()

	return err
}

func ensure(prj string) error {
	cmd := exec.Command("dep", "ensure", "-v", "-update", prj)
	out, err := cmd.CombinedOutput()
	fmt.Println(string(out))
	if err != nil {
		return err
	}

	return nil
}

var docsToCopy = []struct {
	from   []string
	to     []string
	blocks []string
}{
	{
		from: []string{"SUPPORTED.md"},
		to:   []string{"docs", "using-gitbase", "supported-syntax.md"},
	},
	{
		from: []string{"SUPPORTED_CLIENTS.md"},
		to:   []string{"docs", "using-gitbase", "supported-clients.md"},
	},
	{
		from:   []string{"README.md"},
		to:     []string{"docs", "using-gitbase", "functions.md"},
		blocks: []string{"FUNCTIONS"},
	},
	{
		from:   []string{"README.md"},
		to:     []string{"docs", "using-gitbase", "configuration.md"},
		blocks: []string{"CONFIG"},
	},
}

func importDocs() error {
	pwd, err := os.Getwd()
	if err != nil {
		return err
	}
	dirs := strings.Split(goMysqlServer, "/")
	goMysqlServerPath := filepath.Join(append([]string{pwd, "vendor"}, dirs...)...)

	for _, c := range docsToCopy {
		src := filepath.Join(append([]string{goMysqlServerPath}, c.from...)...)
		dst := filepath.Join(append([]string{pwd}, c.to...)...)

		if len(c.blocks) == 0 {
			if err := copyFile(src, dst); err != nil {
				return err
			}
		} else {
			if err := copyFileBlocks(src, dst, c.blocks); err != nil {
				return err
			}
		}
	}

	return nil
}

func copyFile(src, dst string) error {
	fout, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer fout.Close()

	fin, err := os.Open(src)
	if err != nil {
		return err
	}
	defer fin.Close()

	_, err = io.Copy(fout, fin)
	return err
}

func copyFileBlocks(src, dst string, blocks []string) error {
	fout, err := ioutil.ReadFile(dst)
	if err != nil {
		return err
	}

	fin, err := ioutil.ReadFile(src)
	if err != nil {
		return err
	}

	for _, b := range blocks {
		open := []byte(fmt.Sprintf("<!-- BEGIN %s -->", b))
		close := []byte(fmt.Sprintf("<!-- END %s -->", b))

		outOpenIdx := bytes.Index(fout, open)
		outCloseIdx := bytes.Index(fout, close)
		inOpenIdx := bytes.Index(fin, open)
		inCloseIdx := bytes.Index(fin, close)

		if outOpenIdx < 0 || outCloseIdx < 0 {
			return fmt.Errorf("block %q not found on %s", b, dst)
		}

		if inOpenIdx < 0 || inCloseIdx < 0 {
			return fmt.Errorf("block %q not found on %s", b, src)
		}

		var newOut []byte
		newOut = append(newOut, fout[:outOpenIdx]...)
		newOut = append(newOut, []byte(strings.TrimSpace(string(fin[inOpenIdx:inCloseIdx+len(close)])))...)
		newOut = append(newOut, fout[outCloseIdx+len(close):]...)

		fout = newOut
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(fout)
	return err
}
