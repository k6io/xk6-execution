package execution

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.k6.io/k6/core/local"
	"go.k6.io/k6/js"
	"go.k6.io/k6/js/modules"
	"go.k6.io/k6/lib"
	"go.k6.io/k6/lib/testutils"
	"go.k6.io/k6/loader"
	"go.k6.io/k6/stats"
)

func TestMain(m *testing.M) {
	modules.Register("k6/x/execution", New())
	os.Exit(m.Run())
}

func TestExecutionStatsVUSharing(t *testing.T) {
	t.Parallel()
	script := []byte(`
		import exec from 'k6/x/execution';
		import { sleep } from 'k6';

		// The cvus scenario should reuse the two VUs created for the carr scenario.
		export let options = {
			scenarios: {
				carr: {
					executor: 'constant-arrival-rate',
					exec: 'carr',
					rate: 9,
					timeUnit: '0.95s',
					duration: '1s',
					preAllocatedVUs: 2,
					maxVUs: 10,
					gracefulStop: '100ms',
				},
			    cvus: {
					executor: 'constant-vus',
					exec: 'cvus',
					vus: 2,
					duration: '1s',
					startTime: '2s',
					gracefulStop: '0s',
			    },
		    },
		};

		export function cvus() {
			const stats = Object.assign({scenario: 'cvus'}, exec.getVUStats());
			console.log(JSON.stringify(stats));
			sleep(0.2);
		};

		export function carr() {
			const stats = Object.assign({scenario: 'carr'}, exec.getVUStats());
			console.log(JSON.stringify(stats));
		};
`)

	logger := logrus.New()
	logger.SetOutput(ioutil.Discard)
	logHook := testutils.SimpleLogrusHook{HookedLevels: []logrus.Level{logrus.InfoLevel}}
	logger.AddHook(&logHook)

	runner, err := js.New(
		logger,
		&loader.SourceData{
			URL:  &url.URL{Path: "/script.js"},
			Data: script,
		},
		nil,
		lib.RuntimeOptions{},
	)
	require.NoError(t, err)

	ctx, cancel, execScheduler, samples := newTestExecutionScheduler(t, runner, logger, lib.Options{})
	defer cancel()

	type vuStat struct {
		iteration uint64
		scIter    map[string]uint64
	}
	vuStats := map[uint64]*vuStat{}

	type logEntry struct {
		ID, Iteration     uint64
		Scenario          string
		IterationScenario uint64
	}

	errCh := make(chan error, 1)
	go func() { errCh <- execScheduler.Run(ctx, ctx, samples) }()

	select {
	case err := <-errCh:
		require.NoError(t, err)
		entries := logHook.Drain()
		assert.InDelta(t, 20, len(entries), 2)
		le := &logEntry{}
		for _, entry := range entries {
			err = json.Unmarshal([]byte(entry.Message), le)
			require.NoError(t, err)
			assert.Contains(t, []uint64{1, 2}, le.ID)
			if _, ok := vuStats[le.ID]; !ok {
				vuStats[le.ID] = &vuStat{0, make(map[string]uint64)}
			}
			if le.Iteration > vuStats[le.ID].iteration {
				vuStats[le.ID].iteration = le.Iteration
			}
			if le.IterationScenario > vuStats[le.ID].scIter[le.Scenario] {
				vuStats[le.ID].scIter[le.Scenario] = le.IterationScenario
			}
		}
		require.Len(t, vuStats, 2)
		// Both VUs should complete 10 iterations each globally, but 5
		// iterations each per scenario (iterations are 0-based)
		for _, v := range vuStats {
			assert.Equal(t, uint64(9), v.iteration)
			assert.Equal(t, uint64(4), v.scIter["cvus"])
			assert.Equal(t, uint64(4), v.scIter["carr"])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}
}

func TestExecutionStatsScenarioIter(t *testing.T) {
	t.Parallel()
	script := []byte(`
		import exec from 'k6/x/execution';

		// The pvu scenario should reuse the two VUs created for the carr scenario.
		export let options = {
			scenarios: {
				carr: {
					executor: 'constant-arrival-rate',
					exec: 'carr',
					rate: 9,
					timeUnit: '0.95s',
					duration: '1s',
					preAllocatedVUs: 2,
					maxVUs: 10,
					gracefulStop: '100ms',
				},
				pvu: {
					executor: 'per-vu-iterations',
					exec: 'pvu',
					vus: 2,
					iterations: 5,
					startTime: '2s',
					gracefulStop: '100ms',
				},
			},
		};

		export function pvu() {
			const stats = Object.assign({VUID: __VU}, exec.getScenarioStats());
			console.log(JSON.stringify(stats));
		}

		export function carr() {
			const stats = Object.assign({VUID: __VU}, exec.getScenarioStats());
			console.log(JSON.stringify(stats));
		};
`)

	logger := logrus.New()
	logger.SetOutput(ioutil.Discard)
	logHook := testutils.SimpleLogrusHook{HookedLevels: []logrus.Level{logrus.InfoLevel}}
	logger.AddHook(&logHook)

	runner, err := js.New(
		logger,
		&loader.SourceData{
			URL:  &url.URL{Path: "/script.js"},
			Data: script,
		},
		nil,
		lib.RuntimeOptions{},
	)
	require.NoError(t, err)

	ctx, cancel, execScheduler, samples := newTestExecutionScheduler(t, runner, logger, lib.Options{})
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- execScheduler.Run(ctx, ctx, samples) }()

	scStats := map[string]uint64{}

	type logEntry struct {
		Name            string
		Iteration, VUID uint64
	}

	select {
	case err := <-errCh:
		require.NoError(t, err)
		entries := logHook.Drain()
		require.Len(t, entries, 20)
		le := &logEntry{}
		for _, entry := range entries {
			err = json.Unmarshal([]byte(entry.Message), le)
			require.NoError(t, err)
			assert.Contains(t, []uint64{1, 2}, le.VUID)
			if le.Iteration > scStats[le.Name] {
				scStats[le.Name] = le.Iteration
			}
		}
		require.Len(t, scStats, 2)
		// The global per scenario iteration count should be 9 (iterations
		// start at 0), despite VUs being shared or more than 1 being used.
		for _, v := range scStats {
			assert.Equal(t, uint64(9), v)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}
}

// Ensure that scenario iterations returned from k6/x/execution are
// stable during the execution of an iteration.
func TestSharedIterationsStable(t *testing.T) {
	t.Parallel()
	script := []byte(`
		import { sleep } from 'k6';
		import exec from 'k6/x/execution';

		export let options = {
			scenarios: {
				test: {
					executor: 'shared-iterations',
					vus: 50,
					iterations: 50,
				},
			},
		};
		export default function () {
			const stats = exec.getScenarioStats();
			sleep(1);
			console.log(JSON.stringify(Object.assign({VUID: __VU}, stats)));
		}
`)

	logger := logrus.New()
	logger.SetOutput(ioutil.Discard)
	logHook := testutils.SimpleLogrusHook{HookedLevels: []logrus.Level{logrus.InfoLevel}}
	logger.AddHook(&logHook)

	runner, err := js.New(
		logger,
		&loader.SourceData{
			URL:  &url.URL{Path: "/script.js"},
			Data: script,
		},
		nil,
		lib.RuntimeOptions{},
	)
	require.NoError(t, err)

	ctx, cancel, execScheduler, samples := newTestExecutionScheduler(t, runner, logger, lib.Options{})
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- execScheduler.Run(ctx, ctx, samples) }()

	expIters := [50]int64{}
	for i := 0; i < 50; i++ {
		expIters[i] = int64(i)
	}
	gotLocalIters, gotGlobalIters := []int64{}, []int64{}

	type logEntry struct{ Iteration, IterationGlobal int64 }

	select {
	case err := <-errCh:
		require.NoError(t, err)
		entries := logHook.Drain()
		require.Len(t, entries, 50)
		le := &logEntry{}
		for _, entry := range entries {
			err = json.Unmarshal([]byte(entry.Message), le)
			require.NoError(t, err)
			require.Equal(t, le.Iteration, le.IterationGlobal)
			gotLocalIters = append(gotLocalIters, le.Iteration)
			gotGlobalIters = append(gotGlobalIters, le.IterationGlobal)
		}

		assert.ElementsMatch(t, expIters, gotLocalIters)
		assert.ElementsMatch(t, expIters, gotGlobalIters)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestExecutionStats(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name, script, expErr string
	}{
		{name: "vu_ok", script: `
		var exec = require('k6/x/execution');

		exports.default = function() {
			var vuStats = exec.getVUStats();
			if (vuStats.id !== 1) throw new Error('unexpected VU ID: '+vuStats.id);
			if (vuStats.idGlobal !== 10) throw new Error('unexpected global VU ID: '+vuStats.idGlobal);
			if (vuStats.iteration !== 0) throw new Error('unexpected VU iteration: '+vuStats.iteration);
			if (vuStats.iterationScenario !== 0) throw new Error('unexpected scenario iteration: '+vuStats.iterationScenario);
		}`},
		{name: "vu_err", script: `
		var exec = require('k6/x/execution');
		exec.getVUStats();
		`, expErr: "getting VU information in the init context is not supported"},
		{name: "scenario_ok", script: `
		var exec = require('k6/x/execution');
		var sleep = require('k6').sleep;

		exports.default = function() {
			var ss = exec.getScenarioStats();
			sleep(0.1);
			if (ss.name !== 'default') throw new Error('unexpected scenario name: '+ss.name);
			if (ss.executor !== 'test-exec') throw new Error('unexpected executor: '+ss.executor);
			if (ss.startTime > new Date().getTime()) throw new Error('unexpected startTime: '+ss.startTime);
			if (ss.progress !== 0.1) throw new Error('unexpected progress: '+ss.progress);
			if (ss.iteration !== 3) throw new Error('unexpected scenario local iteration: '+ss.iteration);
			if (ss.iterationGlobal !== 4) throw new Error('unexpected scenario local iteration: '+ss.iterationGlobal);
		}`},
		{name: "scenario_err", script: `
		var exec = require('k6/x/execution');
		exec.getScenarioStats();
		`, expErr: "getting scenario information in the init context is not supported"},
		{name: "test_ok", script: `
		var exec = require('k6/x/execution');

		exports.default = function() {
			var ts = exec.getTestInstanceStats();
			if (ts.duration !== 0) throw new Error('unexpected test duration: '+ts.duration);
			if (ts.vusActive !== 1) throw new Error('unexpected vusActive: '+ts.vusActive);
			if (ts.vusMax !== 0) throw new Error('unexpected vusMax: '+ts.vusMax);
			if (ts.iterationsCompleted !== 0) throw new Error('unexpected iterationsCompleted: '+ts.iterationsCompleted);
			if (ts.iterationsInterrupted !== 0) throw new Error('unexpected iterationsInterrupted: '+ts.iterationsInterrupted);
		}`},
		{name: "test_err", script: `
		var exec = require('k6/x/execution');
		exec.getTestInstanceStats();
		`, expErr: "getting test information in the init context is not supported"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := getSimpleRunner(t, "/script.js", tc.script)
			if tc.expErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.expErr)
				return
			}
			require.NoError(t, err)

			samples := make(chan stats.SampleContainer, 100)
			initVU, err := r.NewVU(1, 10, samples)
			require.NoError(t, err)

			execScheduler, err := local.NewExecutionScheduler(r, testutils.NewLogger(t))
			require.NoError(t, err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			ctx = lib.WithExecutionState(ctx, execScheduler.GetState())
			ctx = lib.WithScenarioState(ctx, &lib.ScenarioState{
				Name:      "default",
				Executor:  "test-exec",
				StartTime: time.Now(),
				ProgressFn: func() (float64, []string) {
					return 0.1, nil
				},
			})
			vu := initVU.Activate(&lib.VUActivationParams{
				RunContext:               ctx,
				Exec:                     "default",
				GetNextIterationCounters: func() (uint64, uint64) { return 3, 4 },
			})

			execState := execScheduler.GetState()
			execState.ModCurrentlyActiveVUsCount(+1)
			err = vu.RunOnce()
			assert.NoError(t, err)
		})
	}
}