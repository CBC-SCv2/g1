package scan

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sync"

	"github.com/go-git/go-git/v5"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	log "github.com/sirupsen/logrus"
)

type RepoScanner struct {
	BaseScanner
	repo *git.Repository

	stopChan  chan os.Signal
	leaks     []Leak
	leakChan  chan Leak
	leakWG    *sync.WaitGroup
	leakCache map[string]bool
}

func NewRepoScanner(base BaseScanner, repo *git.Repository) *RepoScanner {
	rs := &RepoScanner{
		BaseScanner: base,
		repo:        repo,
		stopChan:    make(chan os.Signal, 1),
		leakChan:    make(chan Leak),
		leakWG:      &sync.WaitGroup{},
		leakCache:   make(map[string]bool),
	}

	// setup signal stuff
	signal.Notify(rs.stopChan, os.Interrupt)

	go rs.receiveLeaks()

	return rs
}

func (rs *RepoScanner) Scan() error {
	logOpts, err := logOptions(rs.repo, rs.opts)
	if err != nil {
		return err
	}
	cIter, err := rs.repo.Log(logOpts)
	if err != nil {
		return err
	}

	cc := 0
	semaphore := make(chan bool, howManyThreads(rs.opts.Threads))
	wg := sync.WaitGroup{}

	// TODO FINISH REPOSCAN!
	err = cIter.ForEach(func(c *object.Commit) error {
		if c == nil || timeoutReached(rs.ctx) || depthReached(cc, rs.opts) {
			return storer.ErrStop
		}

		// Check if Commit is allowlisted
		if isCommitAllowListed(c.Hash.String(), rs.cfg.Allowlist.Commits) {
			return nil
		}

		// Check if at root
		if len(c.ParentHashes) == 0 {
			cc++
			facScanner := NewFilesAtCommitScanner(rs.BaseScanner, rs.repo, c)
			if err := facScanner.Scan(); err != nil {
				return err
			}
			rs.leaks = append(rs.leaks, facScanner.GetLeaks()...)
			return nil
		}

		// increase Commit counter
		cc++

		// inspect first parent only as all other parents will be eventually reached
		// (they exist as the tip of other branches, etc)
		// See https://github.com/zricethezav/gitleaks/issues/413 for details
		parent, err := c.Parent(0)
		if err != nil {
			return err
		}

		defer func() {
			if err := recover(); err != nil {
				// sometimes the Patch generation will fail due to a known bug in
				// sergi's go-diff: https://github.com/sergi/go-diff/issues/89.
				// Once a fix has been merged I will remove this recover.
				return
			}
		}()

		if timeoutReached(rs.ctx) {
			return nil
		}
		if parent == nil {
			// shouldn't reach this point but just in case
			return nil
		}

		// start := time.Now()
		patch, err := parent.Patch(c)
		if err != nil {
			log.Errorf("could not generate Patch")
		}
		// TODO Record time
		// repo.Manager.RecordTime(manager.PatchTime(howLong(start)))

		wg.Add(1)
		semaphore <- true
		go func(c *object.Commit, patch *object.Patch) {
			defer func() {
				<-semaphore
				wg.Done()
			}()
			for _, f := range patch.FilePatches() {
				if timeoutReached(rs.ctx) {
					return
				}
				if f.IsBinary() {
					continue
				}
				for _, chunk := range f.Chunks() {
					if chunk.Type() == fdiff.Add || (rs.opts.Deletion && chunk.Type() == fdiff.Delete) {
						from, to := f.Files()
						var filepath string
						if from != nil {
							filepath = from.Path()
						} else if to != nil {
							filepath = to.Path()
						} else {
							filepath = "???"
						}
						for _, leak := range checkRules(rs.cfg, "", filepath, c, chunk.Content()) {
							rs.leakWG.Add(1)
							rs.leakChan <- leak
						}
					}
				}
			}
		}(c, patch)

		if c.Hash.String() == rs.opts.CommitTo {
			return storer.ErrStop
		}
		return nil
	})

	wg.Wait()
	// TODO Record Time
	//repo.Manager.RecordTime(manager.ScanTime(howLong(scanTimeStart)))
	//repo.Manager.IncrementCommits(cc)
	fmt.Println("DONE")
	return nil
}

func (rs *RepoScanner) receiveLeaks() {
	for leak := range rs.leakChan {
		rs.leaks = append(rs.leaks, leak)
		if rs.opts.Verbose {
			var b []byte
			if rs.opts.PrettyPrint {
				b, _ = json.MarshalIndent(leak, "", "	")
			} else {
				b, _ = json.Marshal(leak)
			}
			fmt.Println(string(b))
		}
		rs.leakWG.Done()
	}
}

func (rs *RepoScanner) GetLeaks() []Leak {
	fmt.Println("REPORTING")
	rs.leakWG.Wait()
	return rs.leaks
}
