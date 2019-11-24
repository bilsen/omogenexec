package eval

import (
	"fmt"
	"github.com/jsannemo/omogenjudge/util/go/files"
	"github.com/jsannemo/omogenjudge/util/go/users"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	_ "github.com/google/logger"

	runpb "github.com/jsannemo/omogenjudge/runner/api"
	"github.com/jsannemo/omogenjudge/runner/diff"
	"github.com/jsannemo/omogenjudge/runner/runners"
)

type outcome struct {
	verdict runpb.Verdict
	time    int32
}

type Evaluator struct {
	root        string
	linker      *runners.FileLinker
	program     runners.Program
	valLinker   *runners.FileLinker
	validator   runners.Program
	EvaluateAll bool
	EvalCache   map[string]outcome
}

func NewEvaluator(root string, program runners.Program, validator runners.Program) (*Evaluator, error) {
	fl, err := runners.NewFileLinker(filepath.Join(root, "env"))
	if err != nil {
		return nil, fmt.Errorf("failed creating FileLinker: %v", err)
	}
	eval := &Evaluator{
		root:      root,
		linker:    fl,
		program:   program,
		validator: validator,
		EvalCache: make(map[string]outcome),
	}
	if validator != nil {
		valfl, err := runners.NewFileLinker(filepath.Join(root, "valenv"))
		if err != nil {
			return nil, fmt.Errorf("failed creating validator FileLinker: %v", err)
		}
		eval.valLinker = valfl
	}
	return eval, nil
}

type Result struct {
	TimeUsageMs       int32
	Score             int32
	TestCaseVerdict   runpb.Verdict
	TestGroupVerdict  runpb.Verdict
	SubmissionVerdict runpb.Verdict
}

func (e *Evaluator) resetPermissions() error {
	cmd := exec.Command("/usr/bin/omogenjudge-permissionfixer", filepath.Base(e.root))
	return cmd.Run()
}

func (e *Evaluator) Evaluate(testGroups []*runpb.TestGroup, timeLimMs int32, memLimitKb int32, results chan<- *Result) error {
	if err := e.resetPermissions(); err != nil {
		return fmt.Errorf("could not reset permissions: %v", err)
	}
	defer close(results)
	defer e.linker.Clear()
	if e.valLinker != nil {
		defer e.valLinker.Clear()
	}
	outPath := e.linker.PathFor("output", true)
	e.program.SetArgs(&runners.ProgramArgs{
		InputPath:     e.linker.PathFor("input", false),
		OutputPath:    outPath,
		ErrorPath:     e.linker.PathFor("error", true),
		TimeLimitMs:   timeLimMs,
		MemoryLimitKb: memLimitKb,
	})
	if e.validator != nil {
		e.validator.SetArgs(&runners.ProgramArgs{
			InputPath:  e.valLinker.PathFor("team_output", false),
			OutputPath: e.valLinker.PathFor("output", true),
			ErrorPath:  e.valLinker.PathFor("error", true),
			// TODO make this configurable
			TimeLimitMs:   60000,
			MemoryLimitKb: 1000 * 1000,
			ExtraArgs: []string{
				e.valLinker.PathFor("input", false),
				e.valLinker.PathFor("judge_answer", false),
				filepath.Join(e.valLinker.PathFor("feedback", true)),
			},
		})
	}

	verdict := runpb.Verdict_ACCEPTED
	time := int32(0)
	score := int32(0)
	for _, tg := range testGroups {
		groupTime, groupVerdict, err := evaluateGroup(tg, outPath, e, results)
		if err != nil {
			return err
		}
		if groupTime > time {
			time = groupTime
		}
		if groupVerdict != runpb.Verdict_ACCEPTED {
			if verdict == runpb.Verdict_ACCEPTED {
				verdict = groupVerdict
			}
			results <- &Result{TestGroupVerdict: groupVerdict, TimeUsageMs: groupTime, Score: 0}
		} else {
			score += tg.Score
			results <- &Result{TestGroupVerdict: groupVerdict, TimeUsageMs: groupTime, Score: tg.Score}
		}
	}
	if score != 0 {
		verdict = runpb.Verdict_ACCEPTED
	}
	results <- &Result{SubmissionVerdict: verdict, Score: score, TimeUsageMs: time}
	return nil
}

func evaluateGroup(tg *runpb.TestGroup, outPath string, e *Evaluator, results chan<- *Result) (int32, runpb.Verdict, error) {
	groupTime := int32(0)
	verdict := runpb.Verdict_ACCEPTED
	for _, tc := range tg.Cases {
		res, err := evaluateCase(e, tc, outPath)
		if err != nil {
			return groupTime, verdict, err
		}
		results <- &Result{TestCaseVerdict: res.verdict, TimeUsageMs: res.time}
		if res.time > groupTime {
			groupTime = res.time
		}

		if err := e.linker.Clear(); err != nil {
			return groupTime, verdict, err
		}
		if e.valLinker != nil {
			if err := e.valLinker.Clear(); err != nil {
				return groupTime, verdict, err
			}
		}
		if res.verdict != runpb.Verdict_ACCEPTED {
			if verdict == runpb.Verdict_ACCEPTED {
				verdict = res.verdict
			}
			if !e.EvaluateAll {
				break
			}
		}
	}
	return groupTime, verdict, nil
}

func evaluateCase(e *Evaluator, tc *runpb.TestCase, outPath string) (outcome, error) {
	cacheKey := tc.InputPath + " " + tc.OutputPath
	if res, ok := e.EvalCache[cacheKey]; ok {
		return res, nil
	}
	res := outcome{
		time:    int32(0),
		verdict: runpb.Verdict_VERDICT_UNSPECIFIED,
	}
	tcPath := filepath.Join(e.root, tc.Name)
	fb := files.NewFileBase(tcPath)
	fb.Gid = users.OmogenClientsID()
	fb.GroupWritable = true
	if err := os.MkdirAll(tcPath, 0755); err != nil {
		return res, err
	}

	if err := e.linker.LinkFile(tc.InputPath, "input", false); err != nil {
		return res, err
	}
	if err := fb.WriteFile("output", []byte{}); err != nil {
		return res, err
	}
	if err := e.linker.LinkFile(tcPath+"/output", "output", true); err != nil {
		return res, err
	}
	if err := fb.WriteFile("error", []byte{}); err != nil {
		return res, err
	}
	if err := e.linker.LinkFile(tcPath+"/error", "error", true); err != nil {
		return res, err
	}

	exit, err := e.program.Execute()
	if err != nil {
		return res, err
	}
	if err := e.resetPermissions(); err != nil {
		return res, err
	}

	if exit.Crashed() {
		res.verdict = runpb.Verdict_RUN_TIME_ERROR
	} else if exit.TimedOut() {
		res.verdict = runpb.Verdict_TIME_LIMIT_EXCEEDED
	} else {
		wa := false
		if e.validator != nil {
			wa, err = runValidator(tc.InputPath, outPath, tc.OutputPath, e)
			if err != nil {
				return res, err
			}
		} else {
			wa, err = diffOutput(tc.OutputPath, outPath)
			if err != nil {
				return res, err
			}
		}
		if wa {
			res.verdict = runpb.Verdict_WRONG_ANSWER
		} else {
			res.verdict = runpb.Verdict_ACCEPTED
		}
	}
	e.EvalCache[cacheKey] = res
	return res, nil
}

func runValidator(inpath, teampath, anspath string, e *Evaluator) (bool, error) {
	if err := e.valLinker.LinkFile(inpath, "input", false); err != nil {
		return false, err
	}
	if err := e.valLinker.LinkFile(teampath, "team_output", false); err != nil {
		return false, err
	}
	if err := e.valLinker.LinkFile(anspath, "judge_answer", false); err != nil {
		return false, err
	}
	exit, err := e.validator.Execute()
	if err != nil {
		return false, err
	}
	if err := e.resetPermissions(); err != nil {
		return false, err
	}

	if exit.TimedOut() {
		return false, fmt.Errorf("output validator timed out")
	}
	if exit.CrashedWith(42) {
		return false, nil
	}
	if exit.CrashedWith(43) {
		return true, nil
	}
	// Crash was abnormal
	dat, err := ioutil.ReadFile(e.valLinker.PathFor("error", true))
	if err != nil {
		return false, fmt.Errorf("could not read output validator errors: %v", err)
	}
	dat2, err := ioutil.ReadFile(e.valLinker.PathFor("output", true))
	if err != nil {
		return false, fmt.Errorf("could not read output validator output: %v", err)
	}
	return false, fmt.Errorf("output validator crashed: %v", string(dat)+" "+string(dat2))
}

func diffOutput(refPath, outPath string) (bool, error) {
	refFile, err := os.Open(refPath)
	if err != nil {
		return false, err
	}
	outFile, err := os.Open(outPath)
	if err != nil {
		return false, err
	}
	res, err := diff.Diff(refFile, outFile)
	return !res.Match, err
}
