package llbsolver

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	intoto "github.com/in-toto/in-toto-golang/in_toto"
	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/errdefs"
	"github.com/moby/buildkit/executor/resources"
	"github.com/moby/buildkit/exporter"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/frontend/attestations"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/history"
	"github.com/moby/buildkit/solver/llbsolver/provenance"
	provenancetypes "github.com/moby/buildkit/solver/llbsolver/provenance/types"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/tracing"
	"github.com/moby/buildkit/util/tracing/detect"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// recordBuildCompletionWithoutHistory reads an already-closed job's status
// stream and emits the same build metrics as the history-recording path.
func (s *Solver) recordBuildCompletionWithoutHistory(ctx context.Context, j *solver.Job, startedAt time.Time, buildErr error) {
	ctx, cancel := context.WithTimeoutCause(ctx, 300*time.Second, errors.WithStack(context.DeadlineExceeded))
	defer cancel()

	ch := make(chan *client.SolveStatus)
	statusErr := make(chan error, 1)
	go func() {
		statusErr <- j.Status(ctx, ch)
	}()

	rec := &controlapi.BuildHistoryRecord{
		CreatedAt:   timestamppb.New(startedAt),
		CompletedAt: timestamppb.Now(),
	}
	if buildErr != nil {
		rec.Error = history.BuildErrorStatus(ctx, buildErr)
	}

	var summary history.StatusSummary
	for st := range ch {
		summary.Update(st)
	}
	if err := <-statusErr; err != nil {
		bklog.G(ctx).Warnf("failed to read build status for metrics: %+v", err)
	}
	rec.NumCachedSteps = int32(summary.NumCachedSteps)
	rec.NumCompletedSteps = int32(summary.NumCompletedSteps)
	rec.NumTotalSteps = int32(summary.NumTotalSteps)
	rec.NumWarnings = int32(summary.NumWarnings)
	s.metrics.recordBuildCompletion(ctx, rec)
}

func (s *Solver) recordBuildHistory(ctx context.Context, id string, req frontend.SolveRequest, exp ExporterRequest, j *solver.Job, usage *resources.SysSampler) (func(context.Context, *Result, []exporter.DescriptorReference, error) error, error) {
	stopTrace, err := detect.Recorder.Record(ctx)
	if err != nil {
		return nil, errdefs.Internal(err)
	}

	rec := &controlapi.BuildHistoryRecord{
		Ref:           id,
		Frontend:      req.Frontend,
		FrontendAttrs: req.FrontendOpt,
		CreatedAt:     timestamppb.Now(),
	}

	for _, e := range exp.Exporters {
		rec.Exporters = append(rec.Exporters, &controlapi.Exporter{
			Type:  e.Type(),
			Attrs: e.Attrs(),
		})
	}

	if err := s.history.Update(ctx, &controlapi.BuildHistoryEvent{
		Type:   controlapi.BuildHistoryEventType_STARTED,
		Record: rec,
	}); err != nil {
		if stopTrace != nil {
			stopTrace()
		}
		return nil, errdefs.Internal(err)
	}

	return func(ctx context.Context, res *Result, descrefs []exporter.DescriptorReference, err error) error {
		rec.CompletedAt = timestamppb.Now()

		span, ctx := tracing.StartSpan(ctx, "create history record")
		defer span.End()

		j.CloseProgress()

		if res != nil && len(res.Metadata) > 0 {
			rec.ExporterResponse = map[string]string{}
			for k, v := range res.Metadata {
				rec.ExporterResponse[k] = string(v)
			}
		}

		ctx, cancel := context.WithCancelCause(ctx)
		ctx, _ = context.WithTimeoutCause(ctx, 300*time.Second, errors.WithStack(context.DeadlineExceeded)) //nolint:govet
		defer func() { cancel(errors.WithStack(context.Canceled)) }()

		var mu sync.Mutex
		ch := make(chan *client.SolveStatus)
		eg, ctx2 := errgroup.WithContext(ctx)
		var releasers []func()

		attrs := map[string]string{
			"mode":          "max",
			"capture-usage": "true",
		}

		// infer builder-id from user input if available
		if attests, err := attestations.Parse(rec.FrontendAttrs); err == nil {
			if prvAttrs, ok := attests["provenance"]; ok {
				if builderID, ok := prvAttrs["builder-id"]; ok {
					attrs["builder-id"] = builderID
				}
			}
		}

		makeProvenance := func(name string, res solver.ResultProxy, cap *provenance.Capture) (*controlapi.Descriptor, func(), error) {
			span, ctx := tracing.StartSpan(ctx, fmt.Sprintf("create %s history provenance", name))
			defer span.End()

			pc, err := NewProvenanceCreator(ctx2, provenancetypes.ProvenanceSLSA1, cap, res, attrs, j, usage, s.provenanceEnv)
			if err != nil {
				return nil, nil, err
			}
			dt, err := pc.PredicateJSON(ctx)
			if err != nil {
				return nil, nil, err
			}
			w, err := s.history.OpenBlobWriter(ctx, intoto.PayloadType)
			if err != nil {
				return nil, nil, err
			}
			defer func() {
				if w != nil {
					w.Discard()
				}
			}()
			if _, err := w.Write(dt); err != nil {
				return nil, nil, err
			}
			desc, release, err := w.Commit(ctx2)
			if err != nil {
				return nil, nil, err
			}
			w = nil
			return &controlapi.Descriptor{
				Digest:    string(desc.Digest),
				Size:      desc.Size,
				MediaType: desc.MediaType,
				Annotations: map[string]string{
					"in-toto.io/predicate-type": pc.PredicateType(),
				},
			}, release, nil
		}

		if res != nil {
			if res.Ref != nil {
				eg.Go(func() error {
					desc, release, err := makeProvenance("default", res.Ref, res.Provenance.Ref)
					if err != nil {
						return err
					}

					mu.Lock()
					releasers = append(releasers, release)
					if rec.Result == nil {
						rec.Result = &controlapi.BuildResultInfo{}
					}
					rec.Result.Attestations = append(rec.Result.Attestations, desc)
					mu.Unlock()
					return nil
				})
			}

			for k, r := range res.Refs {
				if r == nil {
					continue
				}
				cp := res.Provenance.Refs[k]
				eg.Go(func() error {
					desc, release, err := makeProvenance(k, r, cp)
					if err != nil {
						return err
					}

					mu.Lock()
					releasers = append(releasers, release)
					if rec.Results == nil {
						rec.Results = make(map[string]*controlapi.BuildResultInfo)
					}
					if rec.Results[k] == nil {
						rec.Results[k] = &controlapi.BuildResultInfo{}
					}
					rec.Results[k].Attestations = append(rec.Results[k].Attestations, desc)
					mu.Unlock()
					return nil
				})
			}
		}

		eg.Go(func() error {
			st, releaseStatus, err := s.history.ImportStatus(ctx2, ch)
			if err != nil {
				return err
			}
			mu.Lock()
			releasers = append(releasers, releaseStatus)
			rec.Logs = &controlapi.Descriptor{
				Digest:    string(st.Descriptor.Digest),
				Size:      st.Descriptor.Size,
				MediaType: st.Descriptor.MediaType,
			}
			rec.NumCachedSteps = int32(st.NumCachedSteps)
			rec.NumCompletedSteps = int32(st.NumCompletedSteps)
			rec.NumTotalSteps = int32(st.NumTotalSteps)
			rec.NumWarnings = int32(st.NumWarnings)
			mu.Unlock()
			return nil
		})
		eg.Go(func() error {
			return j.Status(ctx2, ch)
		})

		setDeprecated := true
		for i, descref := range descrefs {
			if descref == nil {
				continue
			}
			deprecate := setDeprecated
			setDeprecated = false
			eg.Go(func() error {
				mu.Lock()
				desc := descref.Descriptor()
				controlDesc := &controlapi.Descriptor{
					Digest:      string(desc.Digest),
					Size:        desc.Size,
					MediaType:   desc.MediaType,
					Annotations: desc.Annotations,
				}
				if rec.Result == nil {
					rec.Result = &controlapi.BuildResultInfo{}
				}
				if rec.Result.Results == nil {
					rec.Result.Results = make(map[int64]*controlapi.Descriptor)
				}
				if deprecate {
					// write the first available descriptor to the deprecated
					// field for legacy clients
					rec.Result.ResultDeprecated = controlDesc
				}
				rec.Result.Results[int64(i)] = controlDesc
				mu.Unlock()
				return nil
			})
		}
		if err1 := eg.Wait(); err == nil {
			// any error from exporting history record is internal
			err = errdefs.Internal(err1)
		}

		defer func() {
			for _, f := range releasers {
				f()
			}
		}()

		if err != nil {
			status, desc, release, err1 := s.history.ImportError(ctx, err)
			if err1 != nil {
				// don't replace the build error with this import error
				bklog.G(ctx).Errorf("failed to import error to build record: %+v", err1)
			} else {
				releasers = append(releasers, release)
			}
			rec.ExternalError = desc
			rec.Error = status
		}

		ready, done := s.history.AcquireFinalizer(rec.Ref)

		// Emit build observability metrics before the history record is
		// finalized. rec is fully populated at this point — Frontend,
		// Error, NumCompletedSteps, CreatedAt, and CompletedAt are set
		// regardless of whether the build succeeded or failed.
		s.metrics.recordBuildCompletion(ctx, rec)

		if err1 := s.history.Update(ctx, &controlapi.BuildHistoryEvent{
			Type:   controlapi.BuildHistoryEventType_COMPLETE,
			Record: rec,
		}); err1 != nil {
			if err == nil {
				err = errdefs.Internal(err1)
			}
		}

		if stopTrace == nil {
			bklog.G(ctx).Warn("no trace recorder found, skipping")
			done()
			return err
		}
		go func() {
			defer done()

			// if there is no finalizer request then stop tracing after 3 seconds
			select {
			case <-time.After(3 * time.Second):
			case <-ready:
			}
			spans := stopTrace()

			if len(spans) == 0 {
				return
			}

			if err := func() error {
				w, err := s.history.OpenBlobWriter(context.TODO(), "application/vnd.buildkit.otlp.json.v0")
				if err != nil {
					return err
				}
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				for _, sp := range spans {
					if err := enc.Encode(sp); err != nil {
						return err
					}
				}

				desc, release, err := w.Commit(context.TODO())
				if err != nil {
					return err
				}
				defer release()

				if err := s.history.UpdateRef(context.TODO(), id, func(rec *controlapi.BuildHistoryRecord) error {
					rec.Trace = &controlapi.Descriptor{
						Digest:    string(desc.Digest),
						MediaType: desc.MediaType,
						Size:      desc.Size,
					}
					return nil
				}); err != nil {
					return err
				}
				return nil
			}(); err != nil {
				bklog.G(ctx).Errorf("failed to save trace for %s: %+v", id, err)
			}
		}()

		return err
	}, nil
}
