package service

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"

	"github.com/google/logger"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runpb "github.com/jsannemo/omogenjudge/runner/api"
	"github.com/jsannemo/omogenjudge/runner/eval"
	"github.com/jsannemo/omogenjudge/runner/language"
	"github.com/jsannemo/omogenjudge/runner/runners"
	execpb "github.com/jsannemo/omogenjudge/sandbox/api"
	eclient "github.com/jsannemo/omogenjudge/sandbox/client"
)

var (
	address = flag.String("run_listen_addr", "127.0.0.1:61811", "The run server address to listen to in the format host:port")
)

type runServer struct {
	languages []*runpb.Language
	exec      execpb.ExecuteServiceClient
}

// Implementation of RunServer.GetLanguages.
func (s *runServer) GetLanguages(ctx context.Context, _ *runpb.GetLanguagesRequest) (*runpb.GetLanguagesResponse, error) {
	return &runpb.GetLanguagesResponse{InstalledLanguages: s.languages}, nil
}

var compileCache = make(map[string]*runpb.CompileResponse)
var cacheLock sync.Mutex

func (s *runServer) CompileCached(ctx context.Context, req *runpb.CompileCachedRequest) (*runpb.CompileCachedResponse, error) {
	cacheLock.Lock()
	defer cacheLock.Unlock()
	res, has := compileCache[req.Identifier]
	if has {
		return &runpb.CompileCachedResponse{Response: res}, nil
	} else {
		res, err := s.Compile(ctx, req.Request)
		if err != nil {
			return nil, err
		}
		compileCache[req.Identifier] = res
		return &runpb.CompileCachedResponse{Response: res}, nil
	}
}

// Implementation of RunServer.Compile.
func (s *runServer) Compile(ctx context.Context, req *runpb.CompileRequest) (*runpb.CompileResponse, error) {
	logger.Infof("RunService.Compile: %v", req)
	// TODO: add request validation
	language, exists := language.GetLanguage(req.Program.LanguageId)
	if !exists {
		return nil, status.Errorf(codes.InvalidArgument, "Language %v does not exist", req.Program.LanguageId)
	}
	compiledProgram, err := language.Compile(req.Program, req.OutputPath, s.exec)
	if err != nil {
		logger.Errorf("Failed program compilation: %v", err)
		return nil, err
	}
	response := &runpb.CompileResponse{
		Program:           compiledProgram.Program,
		CompilationOutput: compiledProgram.Output,
		CompilationError:  compiledProgram.Errors,
	}
	return response, nil
}

// Implementation of RunServer.Evaluate.
func (s *runServer) Evaluate(req *runpb.EvaluateRequest, stream runpb.RunService_EvaluateServer) error {
	execStream, err := s.exec.Execute(context.Background())
	defer execStream.CloseSend()
	if err != nil {
		return err
	}

	lang, exists := language.GetLanguage(req.Program.LanguageId)
	if !exists {
		return status.Errorf(codes.InvalidArgument, "Language %v does not exist", req.Program.LanguageId)
	}
	program, err := lang.Program(req.Program, execStream)
	if err != nil {
		return err
	}
	var validator runners.Program
	if req.Validator != nil {
		valExecStream, err := s.exec.Execute(context.Background())
		defer valExecStream.CloseSend()
		if err != nil {
			return err
		}
		validator, err = lang.Program(req.Validator.Program, valExecStream)
		if err != nil {
			return err
		}
	}

	root := fmt.Sprintf("/var/lib/omogen/submissions/%d", req.SubmissionId)
	if err := os.Mkdir(root, 0755); err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	evaluator, err := eval.NewEvaluator(root, program, validator)
	if err != nil {
		return fmt.Errorf("failed creating evaluator: %v", err)
	}

	var tcs []*eval.TestCase
	for _, tc := range req.Cases {
		tcs = append(tcs, &eval.TestCase{
			Name:       tc.Name,
			InputPath:  tc.InputPath,
			OutputPath: tc.OutputPath,
		})
	}

	results := make(chan *eval.Result, 10)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		for res := range results {
			if res.TestCaseVerdict != runpb.Verdict_VERDICT_UNSPECIFIED {
				stream.Send(&runpb.EvaluateResponse{
					Result: &runpb.EvaluateResponse_TestCase{&runpb.TestCaseResult{Verdict: res.TestCaseVerdict}},
				})
			} else if res.SubmissionVerdict != runpb.Verdict_VERDICT_UNSPECIFIED {
				stream.Send(&runpb.EvaluateResponse{
					Result: &runpb.EvaluateResponse_Submission{&runpb.SubmissionResult{Verdict: res.SubmissionVerdict}},
				})
			}
		}
		wg.Done()
	}()
	if err := evaluator.Evaluate(tcs, req.TimeLimitMs, req.MemLimitKb, results); err != nil {
		return fmt.Errorf("failed evaluation: %v", err)
	}
	wg.Wait()
	return nil
}

func newServer() (*runServer, error) {
	apiLanguages := make([]*runpb.Language, 0)
	for _, language := range language.GetLanguages() {
		apiLanguages = append(apiLanguages, language.ToApiLanguage())
	}
	s := &runServer{
		languages: apiLanguages,
		exec:      eclient.NewClient(),
	}
	return s, nil
}

func Register(grpcServer *grpc.Server) {
	server, err := newServer()
	if err != nil {
		logger.Fatalf("failed to create server: %v", err)
	}
	runpb.RegisterRunServiceServer(grpcServer, server)
}
