// Package conoffsetcollect handles collection of consumer offsets for consumer groups
package conoffsetcollect

import (
	"errors"
	"fmt"
	"sync"

	"github.com/newrelic/infra-integrations-sdk/data/metric"
	"github.com/newrelic/infra-integrations-sdk/integration"
	"github.com/newrelic/infra-integrations-sdk/log"
	"github.com/newrelic/nri-kafka/src/args"
	"github.com/newrelic/nri-kafka/src/zookeeper"
)

type partitionOffsets struct {
	Topic          string `metric_name:"topic" source_type:"attribute"`
	Partition      string `metric_name:"partition" source_type:"attribute"`
	ConsumerOffset *int64 `metric_name:"kafka.consumerOffset" source_type:"gauge"`
	HighWaterMark  *int64 `metric_name:"kafka.highWaterMark" source_type:"gauge"`
	ConsumerLag    *int64 `metric_name:"kafka.consumerLag" source_type:"gauge"`
}

// TopicPartitions is the substructure within the consumer group structure
type TopicPartitions map[string][]int32

// Collect collects offset data per consumer group specified in the arguments
func Collect(zkConn zookeeper.Connection, kafkaIntegration *integration.Integration) error {
	client, err := zkConn.CreateClient()
	if err != nil {
		return err
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Debug("Error closing client connection: %s", err.Error())
		}
	}()

	clusterAdmin, err := zkConn.CreateClusterAdmin()
	if err != nil {
		return err
	}
	defer func() {
		if err := clusterAdmin.Close(); err != nil {
			log.Debug("Error closing clusterAdmin connection: %s", err.Error())
		}
	}()

	// Use the more modern collection method if the configuration exists
	if args.GlobalArgs.ConsumerGroupRegex != nil {
		if err != nil {
			return fmt.Errorf("failed to create cluster admin from client: %s", err)
		}

		consumerGroupMap, err := clusterAdmin.ListConsumerGroups()
		if err != nil {
			return fmt.Errorf("failed to get list of consumer groups: %s", err)
		}
		consumerGroupList := make([]string, len(consumerGroupMap))
		for consumerGroup := range consumerGroupMap {
			consumerGroupList = append(consumerGroupList, consumerGroup)
		}

		consumerGroups, err := clusterAdmin.DescribeConsumerGroups(consumerGroupList)
		if err != nil {
			return fmt.Errorf("failed to get consumer group descriptions: %s", err)
		}

		var unmatchedConsumerGroups []string
		var wg sync.WaitGroup
		numCollected := 0
		skippedConsumerGroups := []string{}
		for _, consumerGroup := range consumerGroups {
			if args.GlobalArgs.ConsumerGroupRegex.MatchString(consumerGroup.GroupId) {
				numCollected++
				if numCollected > 200 {
					skippedConsumerGroups = append(skippedConsumerGroups, consumerGroup.GroupId)
					continue
				}
				wg.Add(1)
				go collectOffsetsForConsumerGroup(client, clusterAdmin, consumerGroup.GroupId, consumerGroup.Members, kafkaIntegration, &wg)
			} else {
				unmatchedConsumerGroups = append(unmatchedConsumerGroups, consumerGroup.GroupId)
			}
		}

		if len(unmatchedConsumerGroups) > 0 {
			log.Debug("Skipped collecting consumer offsets for unmatched consumer groups %v", unmatchedConsumerGroups)
		}

		if len(skippedConsumerGroups) > 0 {
			log.Debug("Reached 200 consumer group limit. Skipping consumer groups %v", skippedConsumerGroups)
		}

		wg.Wait()
	} else if len(args.GlobalArgs.ConsumerGroups) != 0 {
		log.Warn("Argument 'consumer_groups' is deprecated and will be removed in a future version. Use 'consumer_group_regex' instead.")
		// We retrieve the offsets for each group before calculating the high water mark
		// so that the lag is never negative
		for consumerGroup, topics := range args.GlobalArgs.ConsumerGroups {
			topicPartitions := fillTopicPartitions(consumerGroup, topics, client)
			if len(topicPartitions) == 0 {
				log.Error("No topics specified for consumer group '%s'", consumerGroup)
				continue
			}

			offsetData, err := getConsumerOffsets(consumerGroup, topicPartitions, client)
			if err != nil {
				log.Info("Failed to collect consumerOffsets for group %s: %v", consumerGroup, err)
			}
			highWaterMarks, err := getHighWaterMarks(topicPartitions, client)
			if err != nil {
				log.Info("Failed to collect highWaterMarks for group %s: %v", consumerGroup, err)
			}

			offsetStructs := populateOffsetStructs(offsetData, highWaterMarks)

			if err := setMetrics(consumerGroup, offsetStructs, kafkaIntegration); err != nil {
				log.Error("Error setting metrics for consumer group '%s': %s", consumerGroup, err.Error())
			}
		}
	} else {
		return errors.New("if consumer_offset is set, either consumer_group_regex or consumer_groups (deprecated) must also be set")
	}

	return nil
}

// setMetrics adds the metrics from an array of partitionOffsets to the integration
func setMetrics(consumerGroup string, offsetData []*partitionOffsets, kafkaIntegration *integration.Integration) error {
	clusterIDAttr := integration.NewIDAttribute("clusterName", args.GlobalArgs.ClusterName)
	groupEntity, err := kafkaIntegration.Entity(consumerGroup, "ka-consumerGroup", clusterIDAttr)
	if err != nil {
		return err
	}

	for _, offsetData := range offsetData {
		metricSet := groupEntity.NewMetricSet("KafkaOffsetSample",
			metric.Attribute{Key: "displayName", Value: groupEntity.Metadata.Name},
			metric.Attribute{Key: "entityName", Value: "consumerGroup:" + groupEntity.Metadata.Name})

		if err := metricSet.MarshalMetrics(offsetData); err != nil {
			log.Error("Error Marshaling offset metrics for consumer group '%s': %s", consumerGroup, err.Error())
			continue
		}
	}

	return nil
}
