package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"gopkg.in/src-d/go-git.v4/plumbing/object"

	"cloud.google.com/go/datastore"
	"github.com/web-platform-tests/wpt.fyi/shared"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	billy "gopkg.in/src-d/go-billy.v4"
	"gopkg.in/src-d/go-billy.v4/osfs"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/storage"
	"gopkg.in/src-d/go-git.v4/storage/filesystem"
)

var wptGitPath *string
var wptDataPath *string
var projectId *string
var inputGcsBucket *string
var outputGcsBucket *string
var wptdHost *string
var gcpCredentialsFile *string

type byHash []*object.Commit

func (c byHash) Len() int          { return len(c) }
func (c byHash) Swap(i int, j int) { c[i], c[j] = c[j], c[i] }
func (c byHash) Less(i int, j int) bool {
	hi := c[i].Hash[0:]
	hj := c[j].Hash[0:]
	return bytes.Compare(hi, hj) == -1
}

func init() {
	_, srcFilePath, _, ok := runtime.Caller(0)
	if !ok {
		log.Fatal(errors.New("Failed to get golang source file path"))
	}
	defaultGitDir := filepath.Clean(path.Dir(srcFilePath) + "/../../.wpt")
	defaultDataDir := filepath.Clean(path.Dir(srcFilePath) + "/../../.cache/migration")
	wptGitPath = flag.String("wpt_git_path", defaultGitDir, "Path to WPT checkout")
	wptDataPath = flag.String("wpt_data_path", defaultDataDir, "Path to data directory for local data from Google Cloud Storage")
	projectId = flag.String("project_id", "wptdashboard", "Google Cloud Platform project id")
	inputGcsBucket = flag.String("input_gcs_bucket", "wptd", "Google Cloud Storage bucket where shareded test results are stored")
	outputGcsBucket = flag.String("output_gcs_bucket", "wptd-metrics", "Google Cloud Storage bucket where unified test results are stored")
	wptdHost = flag.String("wptd_host", "wpt.fyi", "Hostname of endpoint that serves WPT Dashboard data API")
	gcpCredentialsFile = flag.String("gcp_credentials_file", "client-secret.json", "Path to credentials file for authenticating against Google Cloud Platform services")
}

func getRuns(ctx context.Context, client *datastore.Client) []shared.TestRun {
	query := datastore.NewQuery("TestRun").Order("-CreatedAt")
	testRuns := make([]shared.TestRun, 0)
	it := client.Run(ctx, query)
	for {
		var testRun shared.TestRun
		_, err := it.Next(&testRun)
		if err == iterator.Done {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		testRuns = append(testRuns, testRun)
	}
	return testRuns
}

func getGit(s storage.Storer, fs billy.Filesystem, o *git.CloneOptions) *git.Repository {
	repo, err := git.Open(s, fs)
	if err == git.ErrRepositoryNotExists {
		repo, err = git.Clone(s, fs, o)
		if err != nil {
			log.Fatal(err)
		}
		return nil
	}
	if err != nil {
		log.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		log.Fatal(err)
	}
	if err = wt.Pull(&git.PullOptions{}); err != git.NoErrAlreadyUpToDate && err != nil {
		log.Fatal(err)
	}
	return repo
}

func findHash(commits []*object.Commit, shortHash []byte) int {
	mid := len(commits) / 2
	if len(commits) == 0 {
		return -1
	}
	commitHash := commits[mid].Hash[len(commits[mid].Hash)-5:]
	cmp := bytes.Compare(commitHash, shortHash)
	switch {
	case cmp == 1:
		return findHash(commits[:mid], shortHash)
	case cmp == -1:
		return findHash(commits[mid+1:], shortHash) + mid + 1
	default:
		return mid
	}
}

func getCommitForRuns(repo *git.Repository, runs []shared.TestRun) []*object.Commit {
	log.Println("Gathering commits")
	iter, err := repo.CommitObjects()
	if err != nil {
		log.Fatal(err)
	}
	commits := make([]*object.Commit, 0)
	var commit *object.Commit
	for commit, err = iter.Next(); commit != nil; commit, err = iter.Next() {
		commits = append(commits, commit)
	}
	if err != io.EOF {
		log.Fatal(err)
	}
	log.Println("Sorting commits")
	sort.Sort(byHash(commits))

	log.Println("Matching commits")
	found := make([]*object.Commit, len(runs), len(runs))
	var wg sync.WaitGroup
	wg.Add(len(runs))
	for i, run := range runs {
		go func(i int, run shared.TestRun) {
			defer wg.Done()
			runHash, err := hex.DecodeString(run.Revision)
			if err != nil {
				log.Fatal(err)
			}
			if len(runHash) != 5 {
				log.Fatalf("Unexpected hash length: %d bytes", len(runHash))
			}
			idx := findHash(commits, runHash)
			if idx == -1 {
				log.Printf("Failed to find revision for %s", run.Revision)
				return
			}
			log.Printf("Found commit for %s", run.Revision)
			found[i] = commits[idx]
		}(i, run)
	}
	wg.Wait()

	return found
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	log.Printf("Loading and storing WPT checkout in %s", *wptGitPath)
	log.Printf("Caching WPT data in %s", *wptDataPath)
	err := os.MkdirAll(*wptDataPath, 0755)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	datastoreClient, err := datastore.NewClient(ctx, *projectId, option.WithCredentialsFile(*gcpCredentialsFile))
	if err != nil {
		log.Fatal(err)
	}

	var wg sync.WaitGroup
	var runs []shared.TestRun
	var repo *git.Repository
	wg.Add(2)
	go func() {
		defer wg.Done()
		runs = getRuns(ctx, datastoreClient)
	}()
	go func() {
		defer wg.Done()
		fs := osfs.New(*wptGitPath)
		store, err := filesystem.NewStorage(osfs.New(*wptGitPath + "/.git"))
		if err != nil {
			log.Fatal(err)
		}
		repo = getGit(store, fs, &git.CloneOptions{
			URL: "https://github.com/w3c/web-platform-tests.git",
		})
	}()

	wg.Wait()

	getCommitForRuns(repo, runs)
	//for _, commit := range commits {
	//log.Printf("Found commit %v", commit)
	//}
}
