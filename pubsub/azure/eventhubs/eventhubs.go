/*
Copyright 2023 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package eventhubs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azeventhubs"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azeventhubs/checkpoints"

	azauth "github.com/dapr/components-contrib/internal/authentication/azure"
	"github.com/dapr/components-contrib/pubsub"
	"github.com/dapr/components-contrib/pubsub/azure/eventhubs/conn"
	"github.com/dapr/kit/logger"
)

const (
	defaultMessageRetentionInDays = 1
	defaultPartitionCount         = 1

	resourceCheckMaxRetry         = 5
	resourceCheckMaxRetryInterval = 5 * time.Minute
	resourceCreationTimeout       = 15 * time.Second
	resourceGetTimeout            = 5 * time.Second
)

// AzureEventHubs allows sending/receiving Azure Event Hubs events.
type AzureEventHubs struct {
	metadata *azureEventHubsMetadata
	logger   logger.Logger

	producersLock        *sync.RWMutex
	producers            map[string]*azeventhubs.ProducerClient
	checkpointStoreCache azeventhubs.CheckpointStore
	checkpointStoreLock  *sync.RWMutex
}

// NewAzureEventHubs returns a new Azure Event hubs instance.
func NewAzureEventHubs(logger logger.Logger) pubsub.PubSub {
	return &AzureEventHubs{
		logger:              logger,
		producersLock:       &sync.RWMutex{},
		producers:           make(map[string]*azeventhubs.ProducerClient, 1),
		checkpointStoreLock: &sync.RWMutex{},
	}
}

// Init connects to Azure Event Hubs.
func (aeh *AzureEventHubs) Init(metadata pubsub.Metadata) error {
	m, err := parseEventHubsMetadata(metadata, aeh.logger)
	if err != nil {
		return err
	}
	aeh.metadata = m

	if aeh.metadata.ConnectionString != "" {
		// Connect using the connection string
		var parsedConn *conn.ParsedConn
		parsedConn, err = conn.ParsedConnectionFromStr(aeh.metadata.ConnectionString)
		if err != nil {
			return fmt.Errorf("connectionString is invalid: %w", err)
		}

		if parsedConn.HubName != "" {
			aeh.logger.Infof(`The provided connection string is specific to the Event Hub ("entity path") '%s'; publishing or subscribing to a topic that does not match this Event Hub will fail when attempted`, parsedConn.HubName)
		} else {
			aeh.logger.Infof(`The provided connection string does not contain an Event Hub name ("entity path"); the connection will be established on first publish/subscribe and req.Topic field in incoming requests will be honored`)
		}

		aeh.metadata.hubName = parsedConn.HubName
	} else {
		// Connect via Azure AD
		var env azauth.EnvironmentSettings
		env, err = azauth.NewEnvironmentSettings("eventhubs", metadata.Properties)
		if err != nil {
			return fmt.Errorf("failed to initialize Azure AD credentials: %w", err)
		}
		aeh.metadata.aadTokenProvider, err = env.GetTokenCredential()
		if err != nil {
			return fmt.Errorf("failed to get Azure AD token credentials provider: %w", err)
		}

		aeh.logger.Info("connecting to Azure Event Hub using Azure AD; the connection will be established on first publish/subscribe and req.Topic field in incoming requests will be honored")

		/*if aeh.metadata.EnableEntityManagement {
			if err := aeh.validateEnitityManagementMetadata(); err != nil {
				return err
			}

			// Create hubManager for eventHub management with AAD.
			if managerCreateErr := aeh.createHubManager(); managerCreateErr != nil {
				return managerCreateErr
			}

			// Get Azure Management plane settings for creating consumer groups using event hubs management client.
			settings, err := azauth.NewEnvironmentSettings("azure", metadata.Properties)
			if err != nil {
				return err
			}
			aeh.managementSettings = settings
		}*/
	}

	return nil
}

func (aeh *AzureEventHubs) Features() []pubsub.Feature {
	return nil
}

// Publish sends a message to Azure Event Hubs.
func (aeh *AzureEventHubs) Publish(ctx context.Context, req *pubsub.PublishRequest) error {
	if req.Topic == "" {
		return errors.New("parameter 'topic' is required")
	}

	// Get the producer client
	client, err := aeh.getProducerClientForTopic(ctx, req.Topic)
	if err != nil {
		return fmt.Errorf("error trying to establish a connection: %w", err)
	}

	// Build the batch of messages
	batchOpts := &azeventhubs.EventDataBatchOptions{}
	if pk := req.Metadata["partitionKey"]; pk != "" {
		batchOpts.PartitionKey = &pk
	}
	batch, err := client.NewEventDataBatch(ctx, batchOpts)
	if err != nil {
		return fmt.Errorf("error creating batch: %w", err)
	}
	err = batch.AddEventData(&azeventhubs.EventData{
		Body:        req.Data,
		ContentType: req.ContentType,
	}, nil)
	if err != nil {
		return fmt.Errorf("error adding message to batch: %w", err)
	}

	// Send the message
	client.SendEventDataBatch(ctx, batch, nil)
	if err != nil {
		return fmt.Errorf("error publishing batch: %w", err)
	}

	return nil
}

// BulkPublish sends data to Azure Event Hubs in bulk.
func (aeh *AzureEventHubs) BulkPublish(ctx context.Context, req *pubsub.BulkPublishRequest) (pubsub.BulkPublishResponse, error) {
	return pubsub.BulkPublishResponse{}, nil
}

// Subscribe receives data from Azure Event Hubs.
func (aeh *AzureEventHubs) Subscribe(subscribeCtx context.Context, req pubsub.SubscribeRequest, handler pubsub.Handler) error {
	if aeh.metadata.ConsumerGroup == "" {
		return errors.New("property consumerID is required to subscribe to an Event Hub topic")
	}
	if req.Topic == "" {
		return errors.New("parameter 'topic' is required")
	}

	// Get the processor client
	processor, err := aeh.getProcessorForTopic(subscribeCtx, req.Topic)
	if err != nil {
		return fmt.Errorf("error trying to establish a connection: %w", err)
	}

	// Process all partition clients as they come in
	go func() {
		for {
			// This will block until a new partition client is available
			// It returns nil if processor.Run terminates or if the context is canceled
			partitionClient := processor.NextPartitionClient(subscribeCtx)
			if partitionClient == nil {
				return
			}

			// Once we get a partition client, process the events in a separate goroutine
			go func() {
				processErr := aeh.processEvents(subscribeCtx, partitionClient)
				if processErr != nil {
					aeh.logger.Errorf("Error processing events from partition client: %v", processErr)
				}
			}()
		}
	}()

	// Start the processor
	go func() {
		// This is a blocking call that runs until the context is canceled
		err = processor.Run(subscribeCtx)
		if err != nil {
			aeh.logger.Errorf("Error from event processor: %v", err)
		}
	}()

	return nil
}

func (aeh *AzureEventHubs) processEvents(subscribeCtx context.Context, partitionClient *azeventhubs.ProcessorPartitionClient) error {
	// At the end of the method we need to do some cleanup and close the partition client
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), resourceGetTimeout)
		defer closeCancel()
		closeErr := partitionClient.Close(closeCtx)
		if closeErr != nil {
			aeh.logger.Errorf("Error while closing partition client: %v", closeErr)
		}
	}()

	// Loop to receive messages
	var (
		ctx    context.Context
		cancel context.CancelFunc
		events []*azeventhubs.ReceivedEventData
		err    error
	)
	for {
		// TODO: Support setting a batch size
		const batchSize = 1
		ctx, cancel = context.WithCancel(subscribeCtx)
		events, err = partitionClient.ReceiveEvents(ctx, batchSize, nil)
		cancel()

		// A DeadlineExceeded error means that the context timed out before we received the full batch of messages, and that's fine
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			// If we get an error like ErrorCodeOwnershipLost, it means that the partition was rebalanced and we lost it
			// We'll just stop this subscription and return
			eventHubError := (*azeventhubs.Error)(nil)
			if errors.As(err, &eventHubError) && eventHubError.Code == azeventhubs.ErrorCodeOwnershipLost {
				aeh.logger.Debug("Partition client lost ownership; stopping")
				return nil
			}

			return fmt.Errorf("error receiving events: %w", err)
		}

		// TODO: Comment out
		aeh.logger.Debugf("Received batch with %d events", len(events))

		if len(events) != 0 {
			for _, event := range events {
				// process the event in some way
				fmt.Printf("Event received with body %v\n", event.Body)
			}

			// Update the checkpoint with the last event received. If we lose ownership of this partition or have to restart the next owner will start from this point.
			// This context inherits from the background one in case subscriptionCtx gets canceled
			ctx, cancel = context.WithTimeout(context.Background(), resourceCreationTimeout)
			err = partitionClient.UpdateCheckpoint(ctx, events[len(events)-1])
			cancel()
			if err != nil {
				return fmt.Errorf("failed to update checkpoint: %w", err)
			}
		}
	}
}

func (aeh *AzureEventHubs) Close() (err error) {
	// Acquire locks
	aeh.checkpointStoreLock.Lock()
	defer aeh.checkpointStoreLock.Unlock()
	aeh.producersLock.Lock()
	defer aeh.producersLock.Unlock()

	// Close all producers

	return nil
}

// Returns a producer client for a given topic.
// If the client doesn't exist in the cache, it will create one.
func (aeh *AzureEventHubs) getProducerClientForTopic(ctx context.Context, topic string) (client *azeventhubs.ProducerClient, err error) {
	// Check if we have the producer client in the cache
	aeh.producersLock.RLock()
	client = aeh.producers[topic]
	aeh.producersLock.RUnlock()
	if client != nil {
		return client, nil
	}

	// After acquiring a write lock, check again if the producer exists in the cache just in case another goroutine created it in the meanwhile
	aeh.producersLock.Lock()
	defer aeh.producersLock.Unlock()

	client = aeh.producers[topic]
	if client != nil {
		return client, nil
	}

	// Create a new entity if needed
	if aeh.metadata.EnableEntityManagement {
		// TODO: Create a new entity
	}

	clientOpts := &azeventhubs.ProducerClientOptions{
		ApplicationID: "dapr-" + logger.DaprVersion,
	}

	// Check if we're authenticating using a connection string
	if aeh.metadata.ConnectionString != "" {
		var connString string
		connString, err = aeh.constructConnectionStringFromTopic(topic)
		if err != nil {
			return nil, err
		}
		client, err = azeventhubs.NewProducerClientFromConnectionString(connString, "", clientOpts)
		if err != nil {
			return nil, fmt.Errorf("unable to connect to Azure Event Hub using a connection string: %w", err)
		}
	} else {
		// Use Azure AD
		client, err = azeventhubs.NewProducerClient(aeh.metadata.EventHubNamespace, topic, aeh.metadata.aadTokenProvider, clientOpts)
		if err != nil {
			return nil, fmt.Errorf("unable to connect to Azure Event Hub using Azure AD: %w", err)
		}
	}

	// Store in the cache before returning it
	aeh.producers[topic] = client
	return client, nil
}

// Creates a processor for a given topic.
func (aeh *AzureEventHubs) getProcessorForTopic(ctx context.Context, topic string) (*azeventhubs.Processor, error) {
	// Get the checkpoint store
	checkpointStore, err := aeh.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("unable to connect to the checkpoint store: %w", err)
	}

	// Create a new entity if needed
	if aeh.metadata.EnableEntityManagement {
		// TODO: Create a new entity
	}

	// Create a consumer client
	var consumerClient *azeventhubs.ConsumerClient
	clientOpts := &azeventhubs.ConsumerClientOptions{
		ApplicationID: "dapr-" + logger.DaprVersion,
	}

	// Check if we're authenticating using a connection string
	if aeh.metadata.ConnectionString != "" {
		var connString string
		connString, err = aeh.constructConnectionStringFromTopic(topic)
		if err != nil {
			return nil, err
		}
		consumerClient, err = azeventhubs.NewConsumerClientFromConnectionString(connString, "", aeh.metadata.ConsumerGroup, clientOpts)
		if err != nil {
			return nil, fmt.Errorf("unable to connect to Azure Event Hub using a connection string: %w", err)
		}
	} else {
		// Use Azure AD
		consumerClient, err = azeventhubs.NewConsumerClient(aeh.metadata.EventHubNamespace, topic, aeh.metadata.ConsumerGroup, aeh.metadata.aadTokenProvider, clientOpts)
		if err != nil {
			return nil, fmt.Errorf("unable to connect to Azure Event Hub using Azure AD: %w", err)
		}
	}

	// Create the processor from the consumer client and checkpoint store
	processor, err := azeventhubs.NewProcessor(consumerClient, checkpointStore, nil)
	if err != nil {
		return nil, fmt.Errorf("unable to create the processor: %w", err)
	}

	return processor, nil
}

// Returns the checkpoint store from the object. If it doesn't exist, it lazily initializes it.
func (aeh *AzureEventHubs) getCheckpointStore() (azeventhubs.CheckpointStore, error) {
	// Check if we have the checkpoint store
	aeh.checkpointStoreLock.RLock()
	if aeh.checkpointStoreCache != nil {
		aeh.checkpointStoreLock.RUnlock()
		return aeh.checkpointStoreCache, nil
	}
	aeh.checkpointStoreLock.RUnlock()

	// After acquiring a write lock, check again if the checkpoint store exists in case another goroutine created it in the meanwhile
	aeh.checkpointStoreLock.Lock()
	defer aeh.checkpointStoreLock.Unlock()

	if aeh.checkpointStoreCache != nil {
		return aeh.checkpointStoreCache, nil
	}

	// Init the checkpoint store and store it in the object
	var err error
	aeh.checkpointStoreCache, err = aeh.createCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("unable to connect to the checkpoint store: %w", err)
	}

	return aeh.checkpointStoreCache, nil
}

// Initializes a new checkpoint store
func (aeh *AzureEventHubs) createCheckpointStore() (checkpointStore azeventhubs.CheckpointStore, err error) {
	if aeh.metadata.StorageAccountName == "" {
		return nil, errors.New("property storageAccountName is required to subscribe to an Event Hub topic")
	}
	if aeh.metadata.StorageContainerName == "" {
		return nil, errors.New("property storageContainerName is required to subscribe to an Event Hub topic")
	}

	checkpointStoreOpts := &checkpoints.BlobStoreOptions{
		ClientOptions: policy.ClientOptions{
			Telemetry: policy.TelemetryOptions{
				ApplicationID: "dapr-" + logger.DaprVersion,
			},
		},
	}
	if aeh.metadata.StorageConnectionString != "" {
		// Authenticate with a connection string
		checkpointStore, err = checkpoints.NewBlobStoreFromConnectionString(aeh.metadata.StorageConnectionString, aeh.metadata.StorageContainerName, checkpointStoreOpts)
		if err != nil {
			return nil, fmt.Errorf("error creating checkpointer from connection string: %w", err)
		}
	} else if aeh.metadata.StorageAccountKey != "" {
		// Authenticate with a shared key
		// TODO: This is a workaround in which we assemble a connection string until https://github.com/Azure/azure-sdk-for-go/issues/19842 is fixed
		connString := fmt.Sprintf("DefaultEndpointsProtocol=https;AccountName=%s;AccountKey=%s;EndpointSuffix=core.windows.net", aeh.metadata.StorageAccountName, aeh.metadata.StorageAccountKey)
		checkpointStore, err = checkpoints.NewBlobStoreFromConnectionString(connString, aeh.metadata.StorageContainerName, checkpointStoreOpts)
		if err != nil {
			return nil, fmt.Errorf("error creating checkpointer from storage account credentials: %w", err)
		}
	} else {
		// Use Azure AD
		// If Event Hub is authenticated using a connection string, we can't use Azure AD here
		if aeh.metadata.ConnectionString != "" {
			return nil, errors.New("either one of storageConnectionString or storageAccountKey is required when subscribing to an Event Hub topic without using Azure AD")
		}
		// Use the global URL for Azure Storage
		containerURL := fmt.Sprintf("https://%s.blob.%s/%s", aeh.metadata.StorageAccountName, "core.windows.net", aeh.metadata.StorageContainerName)
		checkpointStore, err = checkpoints.NewBlobStore(containerURL, aeh.metadata.aadTokenProvider, checkpointStoreOpts)
		if err != nil {
			return nil, fmt.Errorf("error creating checkpointer from Azure AD credentials: %w", err)
		}
	}

	return checkpointStore, nil
}

// Returns a connection string with the Event Hub name (entity path) set if not present.
func (aeh *AzureEventHubs) constructConnectionStringFromTopic(topic string) (string, error) {
	if aeh.metadata.hubName != "" {
		if aeh.metadata.hubName != topic {
			return "", fmt.Errorf("the requested topic '%s' does not match the Event Hub name in the connection string", topic)
		}
		return aeh.metadata.ConnectionString, nil
	}

	connString := aeh.metadata.ConnectionString + ";EntityPath=" + topic
	return connString, nil
}
