package srv

import (
	"encoding/json"
	"net/http"

	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/julienschmidt/httprouter"
	"github.com/runconduit/conduit/controller/api/util"
	pb "github.com/runconduit/conduit/controller/gen/public"
	"github.com/runconduit/conduit/pkg/k8s"
	log "github.com/sirupsen/logrus"
)

type (
	jsonError struct {
		Error string `json:"error"`
	}
)

var (
	defaultResourceType = "deployments"
	pbMarshaler         = jsonpb.Marshaler{EmitDefaults: true}
)

func renderJsonError(w http.ResponseWriter, err error, status int) {
	w.Header().Set("Content-Type", "application/json")
	log.Error(err.Error())
	rsp, _ := json.Marshal(jsonError{Error: err.Error()})
	w.WriteHeader(status)
	w.Write(rsp)
}

func renderJson(w http.ResponseWriter, resp interface{}) {
	w.Header().Set("Content-Type", "application/json")
	jsonResp, err := json.Marshal(resp)
	if err != nil {
		renderJsonError(w, err, http.StatusInternalServerError)
		return
	}
	w.Write(jsonResp)
}

func renderJsonPb(w http.ResponseWriter, msg proto.Message) {
	w.Header().Set("Content-Type", "application/json")
	pbMarshaler.Marshal(w, msg)
}

func (h *handler) handleApiVersion(w http.ResponseWriter, req *http.Request, p httprouter.Params) {
	version, err := h.apiClient.Version(req.Context(), &pb.Empty{})

	if err != nil {
		renderJsonError(w, err, http.StatusInternalServerError)
		return
	}
	resp := map[string]interface{}{
		"version": version,
	}
	renderJson(w, resp)
}

func (h *handler) handleApiPods(w http.ResponseWriter, req *http.Request, p httprouter.Params) {
	pods, err := h.apiClient.ListPods(req.Context(), &pb.Empty{})
	if err != nil {
		renderJsonError(w, err, http.StatusInternalServerError)
		return
	}

	renderJsonPb(w, pods)
}

func (h *handler) handleApiPods1(w http.ResponseWriter, req *http.Request, p httprouter.Params) {
	resourceName := req.FormValue("resource_name")
	resourceType := req.FormValue("resource_type")
	namespace := req.FormValue("namespace")

	resourceType, err := k8s.CanonicalKubernetesNameFromFriendlyName(resourceType)
	if err != nil {
		renderJsonError(w, err, http.StatusInternalServerError)
		return
	}

	podReq := &pb.PodSummaryRequest{
		Selector: &pb.ResourceSelection{
			Resource: &pb.Resource{
				Namespace: namespace,
				Name:      resourceName,
				Type:      resourceType,
			},
		},
	}

	result, err := h.apiClient.PodSummary(req.Context(), podReq)
	if err != nil {
		renderJsonError(w, err, http.StatusInternalServerError)
		return
	}
	renderJsonPb(w, result)
}

func (h *handler) handleApiStat(w http.ResponseWriter, req *http.Request, p httprouter.Params) {
	requestParams := util.StatSummaryRequestParams{
		TimeWindow:    req.FormValue("window"),
		ResourceName:  req.FormValue("resource_name"),
		ResourceType:  req.FormValue("resource_type"),
		Namespace:     req.FormValue("namespace"),
		ToName:        req.FormValue("to_name"),
		ToType:        req.FormValue("to_type"),
		ToNamespace:   req.FormValue("to_namespace"),
		FromName:      req.FormValue("from_name"),
		FromType:      req.FormValue("from_type"),
		FromNamespace: req.FormValue("from_namespace"),
	}

	// default to returning deployment stats
	if requestParams.ResourceType == "" {
		requestParams.ResourceType = defaultResourceType
	}

	statRequest, err := util.BuildStatSummaryRequest(requestParams)
	if err != nil {
		renderJsonError(w, err, http.StatusInternalServerError)
		return
	}

	result, err := h.apiClient.StatSummary(req.Context(), statRequest)
	if err != nil {
		renderJsonError(w, err, http.StatusInternalServerError)
		return
	}
	renderJsonPb(w, result)
}
