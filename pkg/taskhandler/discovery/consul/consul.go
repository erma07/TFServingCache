package consul

import (
	"fmt"
	"strconv"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/mKaloer/TFServingCache/pkg/taskhandler"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

type ConsulDiscoveryService struct {
	ListUpdatedChans map[string]chan []taskhandler.ServingService
	ServiceName      string
	ServiceID        string
	ConsulClient     *api.Client
	ttl              time.Duration
	HealthCheckFun   func() (bool, error)
}

func NewDiscoveryService(healthCheck func() (bool, error)) (*ConsulDiscoveryService, error) {
	config := api.DefaultConfig()
	client, err := api.NewClient(config)
	if err != nil {
		return nil, err
	}

	ttl := viper.GetDuration("serviceDiscovery.heartbeatTTL") * time.Second

	serviceId := viper.GetString("serviceDiscovery.consul.serviceId")
	if serviceId == "" {
		serviceId = viper.GetString("serviceDiscovery.consul.serviceName")
	}

	c := &ConsulDiscoveryService{
		ListUpdatedChans: make(map[string]chan []taskhandler.ServingService, 0),
		ConsulClient:     client,
		ttl:              ttl,
		ServiceName:      viper.GetString("serviceDiscovery.consul.serviceName"),
		ServiceID:        serviceId,
		HealthCheckFun:   healthCheck,
	}

	return c, nil
}

func (consul *ConsulDiscoveryService) RegisterService() error {
	agent := consul.ConsulClient.Agent()
	serviceDef := &api.AgentServiceRegistration{
		Name: consul.ServiceName,
		ID:   consul.ServiceID,
		Tags: []string{
			fmt.Sprintf("rest:%d", viper.GetInt("cacheRestPort")),
			fmt.Sprintf("grpc:%d", viper.GetInt("cacheGrpcPort")),
		},
		Check: &api.AgentServiceCheck{
			TTL:                            consul.ttl.String(),
			DeregisterCriticalServiceAfter: (consul.ttl * 100).String(),
		},
	}

	if err := agent.ServiceRegister(serviceDef); err != nil {
		log.WithError(err).Errorf("Could not register consul service")
		return err
	}

	go consul.updateTTL(consul.HealthCheckFun)
	updaterFunc := func() {
		for {
			res, _, err := consul.ConsulClient.Health().Service(consul.ServiceName, "", true, &api.QueryOptions{})
			if err != nil {
				log.WithError(err).Error("Error getting services")
			} else {
				passingNodes := make([]taskhandler.ServingService, 0, len(res))
				for k := range res {
					id := res[k].Service.ID
					grpcPort := 0
					restPort := 0
					for t := range res[k].Service.Tags {
						switch res[k].Service.Tags[t][0:4] {
						case "grpc":
							portStr := res[k].Service.Tags[t][5:]
							grpcPort, err = strconv.Atoi(portStr)
							if err != nil {
								log.WithError(err).Errorf("Invalid grpc port: %s", portStr)
							}
						case "rest":
							portStr := res[k].Service.Tags[t][5:]
							restPort, err = strconv.Atoi(res[k].Service.Tags[t][5:])
							if err != nil {
								log.WithError(err).Errorf("Invalid rest port: %s", portStr)
							}
						}
					}

					addr := res[k].Service.Address
					if addr == "" {
						// Fallback to node addr
						addr = res[k].Node.Address
					}
					log.Debugf("Found node: %s: %s:%s:%s", id, addr, restPort, grpcPort)
					passingNodes = append(passingNodes, taskhandler.ServingService{
						Host:     addr,
						RestPort: restPort,
						GrpcPort: grpcPort,
					})
				}
				for ch := range consul.ListUpdatedChans {
					consul.ListUpdatedChans[ch] <- passingNodes
				}
			}
			time.Sleep(5 * time.Second)
		}
	}
	go updaterFunc()

	return nil
}

func (consul *ConsulDiscoveryService) UnregisterService() error {
	err := consul.ConsulClient.Agent().ServiceDeregister(consul.ServiceID)
	if err != nil {
		log.WithError(err).Errorf("Could not unregister service: %s", consul.ServiceID)
	}
	return err
}

func (consul *ConsulDiscoveryService) AddNodeListUpdated(key string, sub chan []taskhandler.ServingService) {
	consul.ListUpdatedChans[key] = sub
}

func (consul *ConsulDiscoveryService) RemoveNodeListUpdated(key string) {
	delete(consul.ListUpdatedChans, key)
}

func (consul *ConsulDiscoveryService) updateTTL(check func() (bool, error)) {
	ticker := time.NewTicker(consul.ttl / 2)
	for range ticker.C {
		consul.update(check)
	}
}

func (consul *ConsulDiscoveryService) update(check func() (bool, error)) {
	ok, err := check()
	a := consul.ConsulClient.Agent()
	checkId := "service:" + consul.ServiceID

	if !ok {
		log.WithError(err).Warn("Health check failed")
		if agentErr := a.UpdateTTL(checkId, err.Error(), "fail"); agentErr != nil {
			log.WithError(agentErr).Error("Error updating TTL")
		}
	} else {
		if agentErr := a.UpdateTTL(checkId, "", "pass"); agentErr != nil {
			log.WithError(agentErr).Error("Error updating TTL")
		}
	}
}
