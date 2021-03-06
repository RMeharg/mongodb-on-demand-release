package adapter

import (
	"fmt"
	"log"
	"strings"

	"github.com/pivotal-cf/on-demand-services-sdk/bosh"
	"github.com/pivotal-cf/on-demand-services-sdk/serviceadapter"
)

const (
	StemcellAlias           = "mongodb-stemcell"
	MongodInstanceGroupName = "mongod_node"
	MongodJobName           = "mongod_node"
	LifecycleErrandType     = "errand"
)

type ManifestGenerator struct {
	Logger *log.Logger
}

func (m *ManifestGenerator) logf(msg string, v ...interface{}) {
	if m.Logger != nil {
		m.Logger.Printf(msg, v...)
	}
}

func (m ManifestGenerator) GenerateManifest(
	serviceDeployment serviceadapter.ServiceDeployment,
	plan serviceadapter.Plan,
	requestParams serviceadapter.RequestParameters,
	previousManifest *bosh.BoshManifest,
	previousPlan *serviceadapter.Plan) (bosh.BoshManifest, error) {

	m.logf("request params: %#v", requestParams)

	arbitraryParams := requestParams.ArbitraryParams()

	mongoOps := plan.Properties["mongo_ops"].(map[string]interface{})

	username := mongoOps["username"].(string)
	apiKey := mongoOps["api_key"].(string)

	// trim trailing slash
	url := mongoOps["url"].(string)
	url = strings.TrimRight(url, "/")

	oc := &OMClient{Url: url, Username: username, ApiKey: apiKey}

	var previousMongoProperties map[interface{}]interface{}

	if previousManifest != nil {
		previousMongoProperties = mongoPlanProperties(*previousManifest)
	}

	adminPassword, err := passwordForMongoServer(previousMongoProperties)
	if err != nil {
		return bosh.BoshManifest{}, err
	}

	id, err := idForMongoServer(previousMongoProperties)
	if err != nil {
		return bosh.BoshManifest{}, err
	}

	group, err := groupForMongoServer(id, oc, plan.Properties, previousMongoProperties, arbitraryParams)
	if err != nil {
		return bosh.BoshManifest{}, fmt.Errorf("could not create new group (%s)", err.Error())
	}
	m.logf("created group %s", group.ID)

	releases := []bosh.Release{}
	for _, release := range serviceDeployment.Releases {
		releases = append(releases, bosh.Release{
			Name:    release.Name,
			Version: release.Version,
		})
	}

	mongodInstanceGroup := findInstanceGroup(plan, MongodInstanceGroupName)
	if mongodInstanceGroup == nil {
		return bosh.BoshManifest{}, fmt.Errorf("no definition found for instance group '%s'", MongodInstanceGroupName)
	}

	mongodJobs, err := gatherJobs(serviceDeployment.Releases, []string{MongodJobName})
	if err != nil {
		return bosh.BoshManifest{}, err
	}

	mongodNetworks := []bosh.Network{}
	for _, network := range mongodInstanceGroup.Networks {
		mongodNetworks = append(mongodNetworks, bosh.Network{Name: network})
	}
	if len(mongodNetworks) == 0 {
		return bosh.BoshManifest{}, fmt.Errorf("no networks definition found for instance group '%s'", MongodInstanceGroupName)
	}

	configAgentRelease, err := findReleaseForJob(serviceDeployment.Releases, "mongodb_config_agent")
	if err != nil {
		return bosh.BoshManifest{}, err
	}

	engineVersion, ok := arbitraryParams["version"].(string)
	if engineVersion == "" || !ok {
		engineVersion = oc.GetLatestVersion(group.ID)
	}

	// sharded_cluster parameters
	replicas := 0
	routers := 0
	configServers := 0

	// total number of instances
	//
	// standalone:      always one
	// replica_set:     number of replicas
	// sharded_cluster: shards*replicas + config_servers + mongos
	instances := mongodInstanceGroup.Instances

	planID := plan.Properties["id"].(string)
	switch planID {
	case PlanStandalone:
		// ok
	case PlanReplicaSet:
		if r, ok := arbitraryParams["replicas"].(float64); ok && r > 0 {
			instances = int(r)
		}
	case PlanShardedCluster:
		shards := 2
		if s, ok := arbitraryParams["shards"].(float64); ok && s > 0 {
			shards = int(s)
		}

		replicas = 3
		if r, ok := arbitraryParams["replicas"].(float64); ok && r > 0 {
			replicas = int(r)
		}

		configServers = 3
		if c, ok := arbitraryParams["config_servers"].(float64); ok && c > 0 {
			configServers = int(c)
		}

		routers = 2
		if r, ok := arbitraryParams["mongos"].(float64); ok && r > 0 {
			routers = int(r)
		}

		instances = routers + configServers + shards*replicas
	default:
		return bosh.BoshManifest{}, fmt.Errorf("unknown plan: %s", planID)
	}
	authKey, err := GenerateString(512)
	if err != nil {
		return bosh.BoshManifest{}, err
	}

	manifest := bosh.BoshManifest{
		Name:     serviceDeployment.DeploymentName,
		Releases: releases,
		Stemcells: []bosh.Stemcell{
			{
				Alias:   StemcellAlias,
				OS:      serviceDeployment.Stemcell.OS,
				Version: serviceDeployment.Stemcell.Version,
			},
		},
		InstanceGroups: []bosh.InstanceGroup{
			{
				Name:               MongodInstanceGroupName,
				Instances:          instances,
				Jobs:               mongodJobs,
				VMType:             mongodInstanceGroup.VMType,
				VMExtensions:       mongodInstanceGroup.VMExtensions,
				Stemcell:           StemcellAlias,
				PersistentDiskType: mongodInstanceGroup.PersistentDiskType,
				AZs:                mongodInstanceGroup.AZs,
				Networks:           mongodNetworks,
				Properties:         map[string]interface{}{},
			},
			{
				Name:      "mongodb-config-agent",
				Instances: 1,
				Jobs: []bosh.Job{
					{
						Name:    "mongodb_config_agent",
						Release: configAgentRelease.Name,
						Provides: map[string]bosh.ProvidesLink{
							"mongodb_config_agent": {As: "mongodb_config_agent"},
						},
						Consumes: map[string]interface{}{
							"mongod_node": bosh.ConsumesLink{From: "mongod_node"},
						},
					},
				},
				VMType:       mongodInstanceGroup.VMType,
				VMExtensions: mongodInstanceGroup.VMExtensions,
				Stemcell:     StemcellAlias,
				AZs:          mongodInstanceGroup.AZs,
				Networks:     mongodNetworks,

				// See mongodb_config_agent job spec
				Properties: map[string]interface{}{
					"mongo_ops": map[string]interface{}{
						"id":             id,
						"url":            url,
						"agent_api_key":  group.AgentAPIKey,
						"api_key":        apiKey,
						"auth_key":       authKey,
						"username":       username,
						"group_id":       group.ID,
						"plan_id":        planID,
						"admin_password": adminPassword,
						"engine_version": engineVersion,
						"routers":        routers,
						"config_servers": configServers,
						"replicas":       replicas,
					},
				},
			},
			{
				Name:      "cleanup-service",
				Instances: 1,
				Jobs: []bosh.Job{
					{
						Name:    "cleanup_service",
						Release: configAgentRelease.Name,
						Consumes: map[string]interface{}{
							"mongodb_config_agent": bosh.ConsumesLink{From: "mongodb_config_agent"},
						},
					},
				},
				VMType:       mongodInstanceGroup.VMType,
				VMExtensions: mongodInstanceGroup.VMExtensions,
				Stemcell:     StemcellAlias,
				AZs:          mongodInstanceGroup.AZs,
				Networks:     mongodNetworks,
				Lifecycle:    LifecycleErrandType,
				Properties:   map[string]interface{}{},
			},
		},
		Update: bosh.Update{
			Canaries:        1,
			CanaryWatchTime: "3000-180000",
			UpdateWatchTime: "3000-180000",
			MaxInFlight:     4,
		},
		Properties: map[string]interface{}{
			"mongo_ops": map[string]interface{}{
				"url":            url,
				"api_key":        group.AgentAPIKey,
				"group_id":       group.ID,
				"admin_password": adminPassword,

				// options needed for binding
				"plan_id":        planID,
				"routers":        routers,
				"config_servers": configServers,
				"replicas":       replicas,
			},
		},
	}

	m.logf("generated manifest: %#v", manifest)
	return manifest, nil
}

func findInstanceGroup(plan serviceadapter.Plan, jobName string) *serviceadapter.InstanceGroup {
	for _, instanceGroup := range plan.InstanceGroups {
		if instanceGroup.Name == jobName {
			return &instanceGroup
		}
	}
	return nil
}

func gatherJobs(releases serviceadapter.ServiceReleases, requiredJobs []string) ([]bosh.Job, error) {
	jobs := []bosh.Job{}
	for _, requiredJob := range requiredJobs {
		release, err := findReleaseForJob(releases, requiredJob)
		if err != nil {
			return nil, err
		}

		job := bosh.Job{
			Name:    requiredJob,
			Release: release.Name,
			Provides: map[string]bosh.ProvidesLink{
				"mongod_node": {As: "mongod_node"},
			},
			Consumes: map[string]interface{}{
				"mongod_node":          bosh.ConsumesLink{From: "mongod_node"},
				"mongodb_config_agent": bosh.ConsumesLink{From: "mongodb_config_agent"},
			},
		}

		jobs = append(jobs, job)
	}

	return jobs, nil
}

func mongoPlanProperties(manifest bosh.BoshManifest) map[interface{}]interface{} {
	return manifest.InstanceGroups[1].Properties["mongo_ops"].(map[interface{}]interface{})
}

func passwordForMongoServer(previousManifestProperties map[interface{}]interface{}) (string, error) {
	if previousManifestProperties != nil {
		return previousManifestProperties["admin_password"].(string), nil
	}

	return GenerateString(20)
}

func idForMongoServer(previousManifestProperties map[interface{}]interface{}) (string, error) {
	if previousManifestProperties != nil {
		return previousManifestProperties["id"].(string), nil
	}

	return GenerateString(8)
}

func groupForMongoServer(mongoID string, oc *OMClient,
	planProperties map[string]interface{},
	previousManifestProperties map[interface{}]interface{},
	requestParams map[string]interface{}) (Group, error) {

	req := GroupCreateRequest{}
	if name, found := requestParams["projectName"]; found {
		req.Name = name.(string)
	}
	if orgId, found := requestParams["orgId"]; found {
		req.OrgId = orgId.(string)
	}
	tags := planProperties["mongo_ops"].(map[string]interface{})["tags"]
	if tags != nil {
		t := tags.([]interface{})
		for _, tag := range t {
			req.Tags = append(req.Tags, tag.(map[string]interface{})["tag_name"].(string))
		}
	}

	if previousManifestProperties != nil {
		// deleting old group unconditionaly, because  drain script in the tile 0.8.4 version can delete this group at a later time
		// another reason is the because of the bug in 3.6 mongo API agen api key will not be rutrned to us in a result of getGroup request
		// by recreating group we also make sure that all new parameters (like new tags, or new OrgId will be applied)
		err := oc.DeleteGroup(previousManifestProperties["group_id"].(string))
		if err != nil {
			return Group{}, err
		}
	}

	return oc.CreateGroup(mongoID, req)
}

func findReleaseForJob(releases serviceadapter.ServiceReleases, requiredJob string) (serviceadapter.ServiceRelease, error) {
	releasesThatProvideRequiredJob := serviceadapter.ServiceReleases{}

	for _, release := range releases {
		for _, providedJob := range release.Jobs {
			if providedJob == requiredJob {
				releasesThatProvideRequiredJob = append(releasesThatProvideRequiredJob, release)
			}
		}
	}

	if len(releasesThatProvideRequiredJob) == 0 {
		return serviceadapter.ServiceRelease{}, fmt.Errorf("no release provided for job '%s'", requiredJob)
	}

	if len(releasesThatProvideRequiredJob) > 1 {
		releaseNames := []string{}
		for _, release := range releasesThatProvideRequiredJob {
			releaseNames = append(releaseNames, release.Name)
		}

		return serviceadapter.ServiceRelease{}, fmt.Errorf("job '%s' defined in multiple releases: %s", requiredJob, strings.Join(releaseNames, ", "))
	}

	return releasesThatProvideRequiredJob[0], nil
}
