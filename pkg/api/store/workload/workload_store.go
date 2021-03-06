package workload

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/docker/distribution/reference"
	"github.com/rancher/norman/api/access"
	"github.com/rancher/norman/httperror"
	"github.com/rancher/norman/types"
	"github.com/rancher/norman/types/convert"
	"github.com/rancher/norman/types/values"
	"github.com/rancher/rancher/pkg/api/customization/workload"
	"github.com/rancher/rancher/pkg/clustermanager"
	managementschema "github.com/rancher/types/apis/management.cattle.io/v3/schema"
	"github.com/rancher/types/apis/project.cattle.io/v3/schema"
	projectschema "github.com/rancher/types/apis/project.cattle.io/v3/schema"
	managementv3 "github.com/rancher/types/client/management/v3"
	projectclient "github.com/rancher/types/client/project/v3"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
)

func NewWorkloadAggregateStore(schemas *types.Schemas, manager *clustermanager.Manager) {
	workloadSchema := schemas.Schema(&schema.Version, "workload")
	store := NewAggregateStore(schemas.Schema(&schema.Version, "deployment"),
		schemas.Schema(&schema.Version, "replicaSet"),
		schemas.Schema(&schema.Version, "replicationController"),
		schemas.Schema(&schema.Version, "daemonSet"),
		schemas.Schema(&schema.Version, "statefulSet"),
		schemas.Schema(&schema.Version, "job"),
		schemas.Schema(&schema.Version, "cronJob"))

	for _, s := range store.Schemas {
		if s.ID == "deployment" {
			s.Formatter = workload.DeploymentFormatter
		} else {
			s.Formatter = workload.Formatter
		}
	}

	workloadSchema.Store = store
	actionWrapper := workload.ActionWrapper{
		ClusterManager: manager,
	}
	workloadSchema.ActionHandler = actionWrapper.ActionHandler
	workloadSchema.LinkHandler = workload.Handler{}.LinkHandler
}

func NewCustomizeStore(store types.Store) types.Store {
	return &CustomizeStore{
		Store: NewTransformStore(store),
	}
}

type CustomizeStore struct {
	types.Store
}

func (s *CustomizeStore) Create(apiContext *types.APIContext, schema *types.Schema, data map[string]interface{}) (map[string]interface{}, error) {
	setSelector(schema.ID, data)
	setWorkloadSpecificDefaults(schema.ID, data)
	setSecrets(apiContext, data)
	if err := setPorts(convert.ToString(data["name"]), data); err != nil {
		return nil, err
	}
	setScheduling(apiContext, data)
	setStrategy(data)
	return s.Store.Create(apiContext, schema, data)
}

func (s *CustomizeStore) Update(apiContext *types.APIContext, schema *types.Schema, data map[string]interface{}, id string) (map[string]interface{}, error) {
	splitted := strings.Split(id, ":")
	if err := setPorts(splitted[1], data); err != nil {
		return nil, err
	}
	setScheduling(apiContext, data)
	setStrategy(data)
	return s.Store.Update(apiContext, schema, data, id)
}

func (s *CustomizeStore) ByID(apiContext *types.APIContext, schema *types.Schema, id string) (map[string]interface{}, error) {
	shortID := id
	if strings.Count(id, ":") > 1 {
		_, shortID = splitTypeAndID(id)
	}
	return s.Store.ByID(apiContext, schema, shortID)
}

func setScheduling(apiContext *types.APIContext, data map[string]interface{}) {
	if nodeID := convert.ToString(values.GetValueN(data, "scheduling", "node", "nodeId")); nodeID != "" {
		nodeName := getNodeName(apiContext, nodeID)
		values.PutValue(data, nodeName, "scheduling", "node", "nodeId")
		state := getState(data)
		state[getKey(nodeName)] = nodeID
		setState(data, state)
	} else {
		values.PutValue(data, "", "nodeId")
	}
}

func setStrategy(data map[string]interface{}) {
	strategy, ok := values.GetValue(data, "deploymentConfig", "strategy")
	if ok && convert.ToString(strategy) == "Recreate" {
		values.RemoveValue(data, "deploymentConfig", "maxSurge")
		values.RemoveValue(data, "deploymentConfig", "maxUnavailable")
	}
}

func setSelector(schemaID string, data map[string]interface{}) {
	setSelector := false
	isJob := strings.EqualFold(schemaID, "job") || strings.EqualFold(schemaID, "cronJob")
	if convert.IsEmpty(data["selector"]) && !isJob {
		setSelector = true
	}
	if setSelector {
		workloadID := resolveWorkloadID(schemaID, data)
		// set selector
		data["selector"] = map[string]interface{}{
			"matchLabels": map[string]interface{}{
				SelectorLabel: workloadID,
			},
		}

		// set workload labels
		workloadLabels := convert.ToMapInterface(data["workloadLabels"])
		if workloadLabels == nil {
			workloadLabels = make(map[string]interface{})
		}
		workloadLabels[SelectorLabel] = workloadID
		data["workloadLabels"] = workloadLabels

		// set labels
		labels := convert.ToMapInterface(data["labels"])
		if labels == nil {
			labels = make(map[string]interface{})
		}
		labels[SelectorLabel] = workloadID
		data["labels"] = labels
	}
}

func setSecrets(apiContext *types.APIContext, data map[string]interface{}) {
	if val, _ := values.GetValue(data, "imagePullSecrets"); val != nil {
		return
	}
	if containers, _ := values.GetSlice(data, "containers"); len(containers) > 0 {
		imagePullSecrets, _ := data["imagePullSecrets"].([]corev1.LocalObjectReference)
		domainToCreds := getCreds(apiContext, convert.ToString(data["namespaceId"]))
		for _, container := range containers {
			if image := convert.ToString(container["image"]); image != "" {
				domain := getDomain(image)
				if secrets, ok := domainToCreds[domain]; ok {
					imagePullSecrets = append(imagePullSecrets, secrets...)
				}
			}
		}
		if imagePullSecrets != nil {
			values.PutValue(data, imagePullSecrets, "imagePullSecrets")
		}
	}
}

func setWorkloadSpecificDefaults(schemaID string, data map[string]interface{}) {
	if strings.EqualFold(schemaID, "job") || strings.EqualFold(schemaID, "cronJob") {
		// job has different defaults
		if _, ok := data["restartPolicy"]; !ok {
			data["restartPolicy"] = "OnFailure"
		}
	}
}

func setPorts(workloadName string, data map[string]interface{}) error {
	containers, ok := values.GetValue(data, "containers")
	if !ok {
		return nil
	}

	for _, c := range convert.ToInterfaceSlice(containers) {
		cMap, err := convert.EncodeToMap(c)
		if err != nil {
			logrus.Warnf("Failed to transform container to map: %v", err)
			continue
		}
		v, ok := values.GetValue(cMap, "ports")

		if ok {
			ports := convert.ToInterfaceSlice(v)
			usedNames := map[string]bool{}
			for _, p := range ports {
				port, err := convert.EncodeToMap(p)
				if err != nil {
					logrus.Warnf("Failed to transform port to map %v", err)
					continue
				}
				portName := ""
				if convert.IsEmpty(port["name"]) {
					containerPort, err := convert.ToNumber(port["containerPort"])
					if err != nil {
						logrus.Warnf("Failed to transform container port [%v] to number: %v", port["containerPort"], err)
					}
					// port name is of format containerPortProtoSourcePortKind
					// len limit is 15, therefore a) no separator b) kind is numerated
					numKind := 0
					switch kind := convert.ToString(port["kind"]); kind {
					case "NodePort":
						numKind = 1
					case "ClusterIP":
						numKind = 2
					case "LoadBalancer":
						numKind = 3
					}

					portName = fmt.Sprintf("%s%s%s%s", strconv.Itoa(int(containerPort)),
						strings.ToLower(convert.ToString(port["protocol"])),
						strings.ToLower(convert.ToString(port["sourcePort"])),
						strings.ToLower(convert.ToString(numKind)))
				} else {
					portName = convert.ToString(port["name"])
				}

				//validate port name
				if _, ok := usedNames[portName]; ok {
					return httperror.NewAPIError(httperror.InvalidOption, fmt.Sprintf("Duplicated port kind=%v,"+
						" conainerPort=%v, protcol=%v", port["kind"], port["containerPort"], port["protocol"]))
				}
				usedNames[portName] = true
				port["name"] = portName

				if generateDNSName(workloadName, convert.ToString(port["dnsName"])) {
					if port["kind"] == "ClusterIP" {
						// use workload name for clusterIP service as it will be used by dns resolution
						port["dnsName"] = strings.ToLower(convert.ToString(workloadName))
					} else {
						port["dnsName"] = fmt.Sprintf("%s-%s", strings.ToLower(convert.ToString(workloadName)),
							strings.ToLower(convert.ToString(port["kind"])))
					}
				}
			}
		}
	}
	return nil
}

func generateDNSName(workloadName, dnsName string) bool {
	if dnsName == "" {
		return true
	}
	// regenerate the name in case port type got changed
	if strings.EqualFold(dnsName, workloadName) || strings.HasPrefix(dnsName, fmt.Sprintf("%s-", workloadName)) {
		return true
	}
	return false
}

func getCreds(apiContext *types.APIContext, namespaceID string) map[string][]corev1.LocalObjectReference {
	domainToCreds := make(map[string][]corev1.LocalObjectReference)
	var namespacedCreds []projectclient.NamespacedDockerCredential
	if err := access.List(apiContext, &projectschema.Version, "namespacedDockerCredential", &types.QueryOptions{}, &namespacedCreds); err == nil {
		for _, cred := range namespacedCreds {
			if cred.NamespaceId == namespaceID {
				store(cred.Registries, domainToCreds, cred.Name)
			}
		}
	}
	var creds []projectclient.DockerCredential
	if err := access.List(apiContext, &projectschema.Version, "dockerCredential", &types.QueryOptions{}, &creds); err == nil {
		for _, cred := range creds {
			store(cred.Registries, domainToCreds, cred.Name)
		}
	}
	return domainToCreds
}

func getNodeName(apiContext *types.APIContext, nodeID string) string {
	var node managementv3.Node
	var nodeName string
	if err := access.ByID(apiContext, &managementschema.Version, managementv3.NodeType, nodeID, &node); err == nil {
		nodeName = node.NodeName
	}
	return nodeName
}

func setState(data map[string]interface{}, stateMap map[string]string) {
	content, err := json.Marshal(stateMap)
	if err != nil {
		logrus.Errorf("failed to save state on workload: %v", data["id"])
		return
	}

	values.PutValue(data, string(content), "annotations", "workload.cattle.io/state")
}

func getState(data map[string]interface{}) map[string]string {
	state := map[string]string{}

	v, ok := values.GetValue(data, "annotations", "workload.cattle.io/state")
	if ok {
		json.Unmarshal([]byte(convert.ToString(v)), &state)
	}

	return state
}

func getDomain(image string) string {
	var repo string
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		logrus.Debug(err)
		return repo
	}
	domain := reference.Domain(named)
	if domain == "docker.io" {
		return "index.docker.io"
	}
	return domain
}
