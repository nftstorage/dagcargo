package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ipfs/go-cid"
	ipfsapi "github.com/ipfs/go-ipfs-api"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"
)

type stats struct {
	pinned *uint64
	failed *uint64
	refs   *uint64
	size   *uint64
}

var pinDags = &cli.Command{
	Usage: "Pin and analyze DAGs locally",
	Name:  "pin-dags",
	Flags: []cli.Flag{
		&cli.UintFlag{
			Name:  "skip-dags-aged",
			Usage: "If a dag is older than that many days - ignore it",
			Value: 5,
		},
	},
	Action: func(cctx *cli.Context) error {

		var closer func()
		cctx.Context, closer = context.WithCancel(cctx.Context)
		defer closer()

		db, err := connectDb(cctx)
		if err != nil {
			return err
		}
		defer db.Close()

		pinsToDo := make(map[cid.Cid]struct{}, bufPresize)

		rows, err := db.Query(
			cctx.Context,
			`
			SELECT cid_v1 FROM cargo.dags d WHERE
				size_actual IS NULL
					AND
				entry_last_updated > ( NOW() - $1::INTERVAL )
					AND
				EXISTS ( SELECT 42 FROM cargo.dag_sources ds WHERE d.cid_v1 = ds.cid_v1 AND ds.entry_removed IS NULL )
			ORDER BY entry_created DESC -- ensure newest arrivals are attempted first
			`,
			fmt.Sprintf("%d days", cctx.Uint("skip-dags-aged")),
		)
		if err != nil {
			return err
		}
		var cidStr string
		for rows.Next() {
			if err = rows.Scan(&cidStr); err != nil {
				return err
			}
			c, err := cid.Parse(cidStr)
			if err != nil {
				return err
			}
			pinsToDo[c] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			return err
		}

		total := stats{
			pinned: new(uint64),
			failed: new(uint64),
			refs:   new(uint64),
			size:   new(uint64),
		}

		defer func() {
			log.Infow("summary",
				"pinned", atomic.LoadUint64(total.pinned),
				"failed", atomic.LoadUint64(total.failed),
				"referencedBlocks", atomic.LoadUint64(total.refs),
				"bytes", atomic.LoadUint64(total.size),
			)
		}()

		maxWorkers := len(pinsToDo)
		if maxWorkers == 0 {
			return nil
		} else if maxWorkers > cctx.Int("ipfs-api-max-workers") {
			maxWorkers = cctx.Int("ipfs-api-max-workers")
		}

		toPinCh := make(chan cid.Cid, 2*maxWorkers)
		errCh := make(chan error, 1+maxWorkers)

		log.Infof("about to pin and analyze %d dags", len(pinsToDo))

		go func() {
			defer close(toPinCh) // signal to workers to quit

			var progressTick <-chan time.Time
			if showProgress {
				fmt.Fprint(os.Stderr, "0%\r")
				t := time.NewTicker(250 * time.Millisecond)
				progressTick = t.C
				defer t.Stop()
			}

			lastPct := uint64(101)
			for c := range pinsToDo {
				select {
				case toPinCh <- c:
					// feeder
				case <-cctx.Context.Done():
					errCh <- cctx.Context.Err()
					return
				case <-progressTick:
					curPct := 100 * atomic.LoadUint64(total.pinned) / uint64(len(pinsToDo))
					if curPct != lastPct {
						lastPct = curPct
						fmt.Fprintf(os.Stderr, "%d%%\r", lastPct)
					}
				case e := <-errCh:
					if e != nil {
						errCh <- e
					}
					return
				}
			}
		}()

		var wg sync.WaitGroup
		for maxWorkers > 0 {
			maxWorkers--
			wg.Add(1)
			go func() {
				defer wg.Done()

				for {
					c, chanOpen := <-toPinCh
					if !chanOpen {
						return
					}

					if err := pinAndAnalyze(cctx, db, c, total); err != nil {
						errCh <- err
						return
					}
				}
			}()
		}

		wg.Wait()
		if showProgress {
			defer fmt.Fprint(os.Stderr, "100%\n")
		}

		close(errCh)
		return <-errCh
	},
}

type dagStat struct {
	Size      uint64
	NumBlocks uint64
}
type refEntry struct {
	Ref string
	Err string
}

func pinAndAnalyze(cctx *cli.Context, db *pgxpool.Pool, rootCid cid.Cid, total stats) (err error) {

	api := ipfsAPI(cctx)

	// open a tx only when/if we need one, do not hold up pg connections
	var tx pgx.Tx

	defer func() {
		if err == nil {
			err = cctx.Err()
		}

		if err != nil {

			atomic.AddUint64(total.failed, 1)

			if tx != nil {
				tx.Rollback(context.Background()) // nolint:errcheck
			}

			// Timeouts are non-fatal, but still logged as an error
			if ue, castOk := err.(*url.Error); castOk && ue.Timeout() {
				log.Errorf("aborting '%s' of '%s' due to timeout: %s", ue.Op, ue.URL, ue.Unwrap().Error())
				err = nil
			}
		} else if tx != nil {
			err = tx.Commit(cctx.Context)
		}
	}()

	err = api.Request("pin/add").Arguments(rootCid.String()).Exec(cctx.Context, nil)

	// If we fail to even pin - just warn and move on without an error ( we didn't write anything to the DB yet )
	if err != nil {
		log.Warnf("failure to pin %s: %s", rootCid, err)
		atomic.AddUint64(total.failed, 1)
		return nil
	}

	// We got that far: means we have the pin
	// Allow for obscenely long stat/refs times
	api.SetTimeout(time.Second * time.Duration(cctx.Uint("ipfs-api-timeout")) * 15)

	ds := new(dagStat)
	err = api.Request("dag/stat").Arguments(rootCid.String()).Option("progress", "false").Exec(cctx.Context, ds)
	if err != nil {
		return err
	}

	if ds.NumBlocks > 1 {

		refs := make([][]interface{}, 0, 256)

		var resp *ipfsapi.Response
		resp, err = api.Request("refs").Arguments(rootCid.String()).Option("unique", "true").Option("recursive", "true").Send(cctx.Context)

		dec := json.NewDecoder(resp.Output)
		for {
			ref := new(refEntry)
			if decErr := dec.Decode(&ref); decErr != nil {
				if decErr == io.EOF {
					break
				}
				err = decErr
				return err
			}
			if ref.Err != "" {
				err = xerrors.New(ref.Err)
				return err
			}

			var refCid cid.Cid
			refCid, err = cid.Parse(ref.Ref)
			if err != nil {
				return err
			}

			refs = append(refs, []interface{}{
				cidv1(rootCid).String(),
				cidv1(refCid).String(),
			})
		}

		tx, err = db.Begin(cctx.Context)
		if err != nil {
			return err
		}

		_, err = tx.CopyFrom(
			cctx.Context,
			pgx.Identifier{"cargo", "refs"},
			[]string{"cid_v1", "ref_v1"},
			pgx.CopyFromRows(refs),
		)
		if err != nil {
			return err
		}

		atomic.AddUint64(total.refs, uint64(len(refs)))
	}

	updSQL := `UPDATE cargo.dags SET size_actual = $1 WHERE cid_v1 = $2`
	updArgs := []interface{}{ds.Size, cidv1(rootCid).String()}

	if tx != nil {
		_, err = tx.Exec(cctx.Context, updSQL, updArgs...)
	} else {
		_, err = db.Exec(cctx.Context, updSQL, updArgs...)
	}
	if err != nil {
		return err
	}

	atomic.AddUint64(total.pinned, 1)
	atomic.AddUint64(total.size, ds.Size)
	return nil
}
