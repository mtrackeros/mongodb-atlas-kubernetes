package atlas

import (
	"context"
	"fmt"

	"github.com/jedib0t/go-pretty/v6/text"
	"go.mongodb.org/atlas-sdk/v20231001002/admin"
)

func (c *Cleaner) listNetworkPeering(ctx context.Context, projectID string) []admin.BaseNetworkPeeringConnectionSettings {
	peers, _, err := c.client.NetworkPeeringApi.
		ListPeeringConnections(ctx, projectID).
		Execute()
	if err != nil {
		fmt.Println(text.FgRed.Sprintf("\tFailed to list networking peering for project %s: %s", projectID, err))

		return nil
	}

	return peers.Results
}

func (c *Cleaner) getNetworkPeeringContainer(ctx context.Context, projectID, ID string) *admin.CloudProviderContainer {
	container, _, err := c.client.NetworkPeeringApi.GetPeeringContainer(ctx, projectID, ID).Execute()
	if err != nil {
		fmt.Println(text.FgRed.Sprintf("\t\t\tFailed to get network peering container %s: %s", ID, err))

		return nil
	}

	return container
}

func (c *Cleaner) deleteNetworkPeering(ctx context.Context, projectID string, peers []admin.BaseNetworkPeeringConnectionSettings) {
	for _, peer := range peers {
		switch peer.GetProviderName() {
		case CloudProviderAWS:
			container := c.getNetworkPeeringContainer(ctx, projectID, peer.GetContainerId())
			if container == nil {
				continue
			}

			err := c.aws.DeleteVpc(peer.GetVpcId(), container.GetRegionName())
			if err != nil {
				fmt.Println(text.FgRed.Sprintf("\t\t\tFailed to delete VPC %s at region %s from AWS: %s", peer.GetVpcId(), container.GetRegionName(), err))

				continue
			}
		case CloudProviderGCP:
			err := c.gcp.DeleteVpc(ctx, peer.GetNetworkName())
			if err != nil {
				fmt.Println(text.FgRed.Sprintf("\t\t\tFailed to delete VPC %s at project %s from GCP: %s", peer.GetNetworkName(), peer.GetGcpProjectId(), err))

				continue
			}
		case CloudProviderAZURE:
			err := c.azure.DeleteVpc(ctx, peer.GetVnetName())
			if err != nil {
				fmt.Println(text.FgRed.Sprintf("\t\t\tFailed to delete VPC %s from Azure: %s", peer.GetVnetName(), err))

				continue
			}
		}

		_, _, err := c.client.NetworkPeeringApi.DeletePeeringConnection(ctx, projectID, peer.GetId()).Execute()
		if err != nil {
			fmt.Println(text.FgRed.Sprintf("\t\t\tFailed to request deletion of network peering %s: %s", peer.GetId(), err))

			continue
		}

		fmt.Println(text.FgBlue.Sprintf("\t\t\tRequested deletion of network peering %s", peer.GetId()))
	}
}
