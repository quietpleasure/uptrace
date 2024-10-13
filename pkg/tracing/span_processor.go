package tracing

import (
	"context"
	"runtime"
	"slices"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/uptrace/opentelemetry-go-extra/otelzap"
	"github.com/uptrace/uptrace/pkg/attrkey"
	"github.com/uptrace/uptrace/pkg/bunapp"
	"github.com/uptrace/uptrace/pkg/bunotel"
	"github.com/uptrace/uptrace/pkg/org"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
	"go4.org/syncutil"
)

type SpanProcessor struct {
	*bunapp.App

	batchSize int
	queue     chan *Span
	gate      *syncutil.Gate

	sgp *ServiceGraphProcessor

	logger *otelzap.Logger
}

func NewSpanProcessor(app *bunapp.App) *SpanProcessor {
	conf := app.Config()
	maxprocs := runtime.GOMAXPROCS(0)

	p := &SpanProcessor{
		App: app,

		batchSize: conf.Spans.BatchSize,
		queue:     make(chan *Span, conf.Spans.BufferSize),
		gate:      syncutil.NewGate(maxprocs),

		logger: app.Logger,
	}

	if !conf.ServiceGraph.Disabled {
		p.sgp = NewServiceGraphProcessor(app)
	}

	p.logger.Info("starting processing spans...",
		zap.Int("threads", maxprocs),
		zap.Int("batch_size", p.batchSize),
		zap.Int("buffer_size", conf.Spans.BufferSize))

	app.WaitGroup().Add(1)
	go func() {
		defer app.WaitGroup().Done()

		p.processLoop(app.Context())
	}()

	queueLen, _ := bunotel.Meter.Int64ObservableGauge("uptrace.tracing.queue_length",
		metric.WithUnit("{spans}"),
	)

	if _, err := bunotel.Meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			o.ObserveInt64(queueLen, int64(len(p.queue)))
			return nil
		},
		queueLen,
	); err != nil {
		panic(err)
	}

	return p
}

func (p *SpanProcessor) AddSpan(ctx context.Context, span *Span) {
	select {
	case p.queue <- span:
	default:
		p.logger.Error("span buffer is full (consider increasing spans.buffer_size)",
			zap.Int("len", len(p.queue)))
		spanCounter.Add(
			ctx,
			1,
			metric.WithAttributes(
				bunotel.ProjectIDAttr(span.ProjectID),
				attribute.String("type", "dropped"),
			),
		)
	}
}

func (p *SpanProcessor) processLoop(ctx context.Context) {
	const timeout = 5 * time.Second

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	spans := make([]*Span, 0, p.batchSize)

loop:
	for {
		select {
		case span := <-p.queue:
			spans = append(spans, span)

			if len(spans) < p.batchSize {
				break
			}

			p.processSpans(ctx, spans)
			spans = spans[:0]

			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(timeout)
		case <-timer.C:
			if len(spans) > 0 {
				p.processSpans(ctx, spans)
				spans = spans[:0]
			}
			timer.Reset(timeout)
		case <-p.Done():
			break loop
		}
	}

	if len(spans) > 0 {
		p.processSpans(ctx, spans)
	}
}

func (p *SpanProcessor) processSpans(ctx context.Context, src []*Span) {
	ctx, span := bunotel.Tracer.Start(ctx, "process-spans")

	p.WaitGroup().Add(1)
	p.gate.Start()

	spans := make([]*Span, len(src))
	copy(spans, src)

	go func() {
		defer span.End()
		defer p.gate.Done()
		defer p.WaitGroup().Done()

		thread := newSpanProcessorThread(p)
		thread._processSpans(ctx, spans)
	}()
}

func (p *spanProcessorThread) _processSpans(ctx context.Context, spans []*Span) {

	spanConsumer := new(SpanConsumer)
	spanRecorder := &BaseRecorder[*SpanIndex, *SpanData]{
		indexSlice:  make([]*SpanIndex, 0, len(spans)),
		dataSlice:   make([]*SpanData, 0, len(spans)),
		processItem: spanConsumer.processItem,
	}
	spanConsumer.BaseRecorder = spanRecorder

	logConsumer := new(LogConsumer)
	logRecorder := &BaseRecorder[*LogIndex, *LogData]{
		indexSlice:  make([]*LogIndex, 0, len(spans)),
		dataSlice:   make([]*LogData, 0, len(spans)),
		processItem: logConsumer.processItem,
	}
	logConsumer.BaseRecorder = logRecorder

	seenErrors := make(map[uint64]bool) // basic deduplication
	var (
		errorCount int
		logCount   int
	)

	for _, span := range spans {
		p.initSpanOrEvent(ctx, span)
		spanCounter.Add(
			ctx,
			1,
			metric.WithAttributes(
				bunotel.ProjectIDAttr(span.ProjectID),
				attribute.String("type", "inserted"),
			),
		)
		if p.sgp != nil {
			p.WaitGroup().Add(1)
			go func() {
				defer p.WaitGroup().Done()
				index := new(SpanIndex)
				index.init(span)
				if err := p.sgp.ProcessSpan(ctx, index); err != nil {
					p.Zap(ctx).Error("service graph failed", zap.Error(err))
				}
			}()
		}

		if span.EventName != "" {
			logRecorder.processItem(ctx, span)
			continue
		}

		spanRecorder.processItem(ctx, span)
		numIndexInSlice := len(spanRecorder.indexSlice)-1
		for _, event := range span.Events {
			eventSpan := &Span{
				Attrs: NewAttrMap(),
			}
			initEventFromHostSpan(eventSpan, event, span)
			p.initEvent(ctx, eventSpan)

			spanCounter.Add(
				ctx,
				1,
				metric.WithAttributes(
					bunotel.ProjectIDAttr(span.ProjectID),
					attribute.String("type", "inserted"),
				),
			)
			if eventSpan.Type == TypeLog {
				logRecorder.processItem(ctx, eventSpan)
			} else {
				spanRecorder.processItem(ctx, eventSpan)
			}
			
			if isErrorSystem(eventSpan.System) {
				errorCount++
				if !seenErrors[eventSpan.GroupID] {
					seenErrors[eventSpan.GroupID] = true
					scheduleCreateErrorAlert(ctx, p.App, eventSpan)
				}
			}
			if isLogSystem(eventSpan.System) {
				logCount++
			}
		}

		baseSpanIndex := spanRecorder.indexSlice[numIndexInSlice]
		baseSpanIndex.LinkCount = uint8(len(span.Links))
		baseSpanIndex.EventCount = uint8(len(span.Events))
		baseSpanIndex.EventErrorCount = uint8(errorCount)
		baseSpanIndex.EventLogCount = uint8(logCount)

		span.Events = nil
	}

	p.insertCH(ctx, "spans_data", &spanRecorder.dataSlice)
	p.insertCH(ctx, "spans_index", &spanRecorder.indexSlice)
	p.insertCH(ctx, "logs_data", &logRecorder.dataSlice)
	p.insertCH(ctx, "logs_index", &logRecorder.indexSlice)
}

func (p *spanProcessorThread) insertCH(ctx context.Context, table string, model any) {
	if _, err := p.CH.NewInsert().
		Model(model).
		Exec(ctx); err != nil {
		p.Zap(ctx).Error("ch.Insert failed",
			zap.Error(err),
			zap.String("table", table))
	}
}

func scheduleCreateErrorAlert(ctx context.Context, app *bunapp.App, span *Span) {
	job := org.CreateErrorAlertTask.NewJob(
		span.ProjectID,
		span.GroupID,
		span.TraceID,
		span.ID,
	)
	job.OnceInPeriod(15*time.Minute, span.GroupID)
	job.SetDelay(time.Minute)

	if err := app.MainQueue.AddJob(ctx, job); err != nil {
		app.Zap(ctx).Error("MainQueue.Add failed", zap.Error(err))
	}
}

//------------------------------------------------------------------------------

type spanProcessorThread struct {
	*SpanProcessor

	projects map[uint32]*org.Project
	digest   *xxhash.Digest
}

func newSpanProcessorThread(p *SpanProcessor) *spanProcessorThread {
	return &spanProcessorThread{
		SpanProcessor: p,

		projects: make(map[uint32]*org.Project),
		digest:   xxhash.New(),
	}
}

func (p *spanProcessorThread) project(ctx context.Context, projectID uint32) (*org.Project, bool) {
	if project, ok := p.projects[projectID]; ok {
		return project, true
	}

	project, err := org.SelectProject(ctx, p.App, projectID)
	if err != nil {
		p.Zap(ctx).Error("SelectProject failed", zap.Error(err))
		return nil, false
	}

	p.projects[projectID] = project
	return project, true
}

func (p *spanProcessorThread) forceSpanName(ctx context.Context, span *Span) bool {
	if span.EventName != "" {
		return false
	}

	project, ok := p.project(ctx, span.ProjectID)
	if !ok {
		return false
	}

	if libName, _ := span.Attrs[attrkey.OtelLibraryName].(string); libName != "" {
		return slices.Contains(project.ForceSpanName, libName)
	}
	return false
}
