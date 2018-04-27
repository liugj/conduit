package public

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"github.com/runconduit/conduit/controller/api/util"
	pb "github.com/runconduit/conduit/controller/gen/public"
	"github.com/runconduit/conduit/pkg/k8s"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
)

type promType string
type promResult struct {
	prom promType
	vec  model.Vector
	err  error
}

const (
	reqQuery             = "sum(increase(response_total%s[%s])) by (%s, classification)"
	latencyQuantileQuery = "histogram_quantile(%s, sum(irate(response_latency_ms_bucket%s[%s])) by (le, %s))"

	promRequests   = promType("QUERY_REQUESTS")
	promLatencyP50 = promType("0.5")
	promLatencyP95 = promType("0.95")
	promLatencyP99 = promType("0.99")

	namespaceLabel    = model.LabelName("namespace")
	dstNamespaceLabel = model.LabelName("dst_namespace")
)

var promTypes = []promType{promRequests, promLatencyP50, promLatencyP95, promLatencyP99}

type meshedCount struct {
	inMesh uint64
	total  uint64
}

func (s *grpcServer) PodSummary(ctx context.Context, req *pb.PodSummaryRequest) (*pb.PodSummaryResponse, error) {
	objects, err := s.lister.GetObjects(req.Selector.Resource.Namespace, req.Selector.Resource.Type, req.Selector.Resource.Name)
	if err != nil {
		return nil, util.GRPCError(err)
	}

	var rows []*pb.PodList_Row

	for _, object := range objects {
		ns, name, kind, _, err := s.lister.GetSelectorFor(object)
		resource := &pb.Resource{
			Namespace: ns,
			Type:      kind,
			Name:      name,
		}

		statuses, err := s.getPodStatuses(object)
		if err != nil {
			return nil, util.GRPCError(err)
		}

		rows = append(rows, &pb.PodList_Row{
			Resource:    resource,
			PodStatuses: statuses,
		})
	}

	rsp := &pb.PodSummaryResponse{
		Response: &pb.PodSummaryResponse_Ok_{ // https://github.com/golang/protobuf/issues/205
			Ok: &pb.PodSummaryResponse_Ok{
				PodList: &pb.PodList{
					Rows: rows,
				},
			},
		},
	}

	return rsp, nil
}

func (s *grpcServer) StatSummary(ctx context.Context, req *pb.StatSummaryRequest) (*pb.StatSummaryResponse, error) {
	// special case to check for services as outbound only
	if req.Selector.Resource.Type == k8s.Services &&
		req.Outbound.(*pb.StatSummaryRequest_FromResource) == nil {
		return nil, status.Errorf(codes.InvalidArgument, "service only supported as a target on 'from' queries, or as a destination on 'to' queries.")
	}

	objects, err := s.lister.GetObjects(req.Selector.Resource.Namespace, req.Selector.Resource.Type, req.Selector.Resource.Name)
	if err != nil {
		return nil, util.GRPCError(err)
	}

	// TODO: make these one struct:
	// string => {metav1.ObjectMeta, meshedCount}
	objectMap := map[string]metav1.Object{}
	meshCountMap := map[string]*meshedCount{}

	for _, object := range objects {
		key, err := cache.MetaNamespaceKeyFunc(object)
		if err != nil {
			return nil, util.GRPCError(err)
		}
		metaObj, err := meta.Accessor(object)
		if err != nil {
			return nil, util.GRPCError(err)
		}

		objectMap[key] = metaObj

		meshCount, err := s.getMeshedPodCount(object)
		if err != nil {
			return nil, util.GRPCError(err)
		}
		meshCountMap[key] = meshCount
	}

	res, err := s.objectQuery(ctx, req, objectMap, meshCountMap)
	if err != nil {
		return nil, util.GRPCError(err)
	}

	return res, nil
}

func (s *grpcServer) objectQuery(
	ctx context.Context,
	req *pb.StatSummaryRequest,
	objects map[string]metav1.Object,
	meshCount map[string]*meshedCount,
) (*pb.StatSummaryResponse, error) {
	rows := make([]*pb.StatTable_PodGroup_Row, 0)

	requestMetrics, err := s.getRequests(ctx, req, req.TimeWindow)
	if err != nil {
		return nil, err
	}

	var keys []string

	if req.GetOutbound() == nil || req.GetNone() != nil {
		// if this request doesn't have outbound filtering, return all rows
		for key := range objects {
			keys = append(keys, key)
		}
	} else {
		// otherwise only return rows for which we have stats
		for key := range requestMetrics {
			keys = append(keys, key)
		}
	}

	for _, key := range keys {
		resource, ok := objects[key]
		if !ok {
			continue
		}

		row := pb.StatTable_PodGroup_Row{
			Resource: &pb.Resource{
				Namespace: resource.GetNamespace(),
				Type:      req.Selector.Resource.Type,
				Name:      resource.GetName(),
			},
			TimeWindow: req.TimeWindow,
			Stats:      requestMetrics[key],
		}

		if count, ok := meshCount[key]; ok {
			row.MeshedPodCount = count.inMesh
			row.TotalPodCount = count.total
		}

		rows = append(rows, &row)
	}

	rsp := pb.StatSummaryResponse{
		Response: &pb.StatSummaryResponse_Ok_{ // https://github.com/golang/protobuf/issues/205
			Ok: &pb.StatSummaryResponse_Ok{
				StatTables: []*pb.StatTable{
					&pb.StatTable{
						Table: &pb.StatTable_PodGroup_{
							PodGroup: &pb.StatTable_PodGroup{
								Rows: rows,
							},
						},
					},
				},
			},
		},
	}

	return &rsp, nil
}

func promLabelNames(resource *pb.Resource) model.LabelNames {
	names := model.LabelNames{namespaceLabel}
	if resource.Type != k8s.Namespaces {
		names = append(names, promResourceType(resource))
	}
	return names
}

func promDstLabelNames(resource *pb.Resource) model.LabelNames {
	names := model.LabelNames{dstNamespaceLabel}
	if resource.Type != k8s.Namespaces {
		names = append(names, "dst_"+promResourceType(resource))
	}
	return names
}

func promLabels(resource *pb.Resource) model.LabelSet {
	set := model.LabelSet{}
	if resource.Name != "" {
		set[promResourceType(resource)] = model.LabelValue(resource.Name)
	}
	if resource.Type != k8s.Namespaces && resource.Namespace != "" {
		set[namespaceLabel] = model.LabelValue(resource.Namespace)
	}
	return set
}

func promDstLabels(resource *pb.Resource) model.LabelSet {
	set := model.LabelSet{}
	if resource.Name != "" {
		set["dst_"+promResourceType(resource)] = model.LabelValue(resource.Name)
	}
	if resource.Type != k8s.Namespaces && resource.Namespace != "" {
		set[dstNamespaceLabel] = model.LabelValue(resource.Namespace)
	}
	return set
}

func promDirectionLabels(direction string) model.LabelSet {
	return model.LabelSet{
		model.LabelName("direction"): model.LabelValue(direction),
	}
}

func promResourceType(resource *pb.Resource) model.LabelName {
	return model.LabelName(k8s.ResourceTypesToProxyLabels[resource.Type])
}

func buildRequestLabels(req *pb.StatSummaryRequest) (model.LabelSet, model.LabelNames) {
	var labelNames model.LabelNames
	labels := model.LabelSet{}

	switch out := req.Outbound.(type) {
	case *pb.StatSummaryRequest_ToResource:
		labelNames = promLabelNames(req.Selector.Resource)
		labels = labels.Merge(promDstLabels(out.ToResource))
		labels = labels.Merge(promLabels(req.Selector.Resource))
		labels = labels.Merge(promDirectionLabels("outbound"))

	case *pb.StatSummaryRequest_FromResource:
		labelNames = promDstLabelNames(req.Selector.Resource)
		labels = labels.Merge(promLabels(out.FromResource))
		labels = labels.Merge(promDirectionLabels("outbound"))

	default:
		labelNames = promLabelNames(req.Selector.Resource)
		labels = labels.Merge(promLabels(req.Selector.Resource))
		labels = labels.Merge(promDirectionLabels("inbound"))

	}

	return labels, labelNames
}

func (s *grpcServer) getRequests(ctx context.Context, req *pb.StatSummaryRequest, timeWindow string) (map[string]*pb.BasicStats, error) {
	reqLabels, groupBy := buildRequestLabels(req)
	resultChan := make(chan promResult)

	// kick off 4 asynchronous queries: 1 request volume + 3 latency
	go func() {
		requestsQuery := fmt.Sprintf(reqQuery, reqLabels, timeWindow, groupBy)
		resultVector, err := s.queryProm(ctx, requestsQuery)

		resultChan <- promResult{
			prom: promRequests,
			vec:  resultVector,
			err:  err,
		}
	}()

	for _, quantile := range []promType{promLatencyP50, promLatencyP95, promLatencyP99} {
		go func(quantile promType) {
			latencyQuery := fmt.Sprintf(latencyQuantileQuery, quantile, reqLabels, timeWindow, groupBy)
			latencyResult, err := s.queryProm(ctx, latencyQuery)

			resultChan <- promResult{
				prom: quantile,
				vec:  latencyResult,
				err:  err,
			}
		}(quantile)
	}

	// process results, receive one message per prometheus query type
	var err error
	results := []promResult{}
	for i := 0; i < len(promTypes); i++ {
		result := <-resultChan
		if result.err != nil {
			log.Errorf("queryProm failed with: %s", result.err)
			err = result.err
		} else {
			results = append(results, result)
		}
	}
	if err != nil {
		return nil, err
	}

	return processRequests(results, groupBy), nil
}

func processRequests(results []promResult, groupBy model.LabelNames) map[string]*pb.BasicStats {
	basicStats := make(map[string]*pb.BasicStats)

	for _, result := range results {
		for _, sample := range result.vec {
			label := metricToKey(sample.Metric, groupBy)
			if basicStats[label] == nil {
				basicStats[label] = &pb.BasicStats{}
			}

			value := uint64(0)
			if !math.IsNaN(float64(sample.Value)) {
				value = uint64(math.Round(float64(sample.Value)))
			}

			switch result.prom {
			case promRequests:
				switch string(sample.Metric[model.LabelName("classification")]) {
				case "success":
					basicStats[label].SuccessCount = value
				case "failure":
					basicStats[label].FailureCount = value
				}
			case promLatencyP50:
				basicStats[label].LatencyMsP50 = value
			case promLatencyP95:
				basicStats[label].LatencyMsP95 = value
			case promLatencyP99:
				basicStats[label].LatencyMsP99 = value
			}
		}
	}

	return basicStats
}

func metricToKey(metric model.Metric, groupBy model.LabelNames) string {
	values := []string{}
	for _, k := range groupBy {
		values = append(values, string(metric[k]))
	}
	return strings.Join(values, "/")
}

func (s *grpcServer) getPodStatuses(obj runtime.Object) ([]*pb.PodList_Row_PodStatus, error) {
	pods, err := s.lister.GetPodsFor(obj)
	if err != nil {
		return nil, err
	}

	podStatuses := make([]*pb.PodList_Row_PodStatus, 0)
	for _, pod := range pods {
		controllerComponent := pod.Labels[k8s.ControllerComponentLabel]

		status := string(pod.Status.Phase)
		if pod.DeletionTimestamp != nil {
			status = "Terminating"
		}

		podStatuses = append(podStatuses, &pb.PodList_Row_PodStatus{
			Name:         pod.Name,
			Added:        isInMesh(pod),
			ControlPlane: controllerComponent != "",
			Status:       status,
		})
	}

	return podStatuses, nil
}

func (s *grpcServer) getMeshedPodCount(obj runtime.Object) (*meshedCount, error) {
	pods, err := s.lister.GetPodsFor(obj)
	if err != nil {
		return nil, err
	}

	meshCount := &meshedCount{}
	for _, pod := range pods {
		meshCount.total++
		if isInMesh(pod) {
			meshCount.inMesh++
		}
	}

	return meshCount, nil
}

func isInMesh(pod *apiv1.Pod) bool {
	_, ok := pod.Annotations[k8s.ProxyVersionAnnotation]
	return ok
}

func (s *grpcServer) queryProm(ctx context.Context, query string) (model.Vector, error) {
	log.Debugf("Query request: %+v", query)

	// single data point (aka summary) query
	res, err := s.prometheusAPI.Query(ctx, query, time.Time{})
	if err != nil {
		log.Errorf("Query(%+v) failed with: %+v", query, err)
		return nil, err
	}
	log.Debugf("Query response: %+v", res)

	if res.Type() != model.ValVector {
		err = fmt.Errorf("Unexpected query result type (expected Vector): %s", res.Type())
		log.Error(err)
		return nil, err
	}

	return res.(model.Vector), nil
}
