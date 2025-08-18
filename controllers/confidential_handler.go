package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// kata-cc runtime class for CoCo
	kataCCRuntimeClassName        = "kata-cc"
	kataCCRuntimeClassCpuOverhead = "0.25"
	kataCCRuntimeClassMemOverhead = "350Mi"

	// TEE node labels
	intelTDXNodeLabel = "intel.feature.node.kubernetes.io/tdx"
	amdSNPNodeLabel   = "amd.feature.node.kubernetes.io/snp"

	// RuntimeClass handlers for TEE
	kataCCIntelHandler = "kata-cc-intel"
	kataCCAmdHandler   = "kata-cc-amd"
)

// When the feature is enabled, handleFeatureConfidential sets config maps to confidential values.
//
// Changes the ImageConfigMap, so that the image creation job will create a confidential image.
// This will happen at the first reconciliation loop, before the image creation job starts.
//
// Changes the peer pods configMap to enable confidential.
// This will happen likely after several reconciliation loops, because it has prerequisites:
//
//   - Peer pods must be enabled in the KataConfig.
//   - The peer pods config map must exist.
//
// When the feature is disabled, handleFeatureConfidential resets the config maps to non-confidential values.
func (r *KataConfigOpenShiftReconciler) handleFeatureConfidential(state FeatureGateState) error {

	// ImageConfigMap

	if err := InitializeImageGenerator(r.Client); err != nil {
		return err
	}
	ig := GetImageGenerator()

	if ig.provider == unsupportedCloudProvider {
		r.Log.Info("unsupported cloud provider, skipping confidential image configuration")
	} else {
		if ig.isImageIDSet() {
			r.Log.Info("Image ID is already set, skipping confidential image configuration")
		} else {
			if state == Enabled {
				// Create ImageConfigMap, if it doesn't exist already.
				if err := ig.createImageConfigMapFromFile(); err != nil {
					return err
				}

				// Patch ImageConfigMap.
				imageConfigMapData := map[string]string{"CONFIDENTIAL_COMPUTE_ENABLED": "yes"}
				if err := updateConfigMap(r.Client, r.Log, ig.getImageConfigMapName(), OperatorNamespace, imageConfigMapData); err != nil {
					return err
				}
			} else {
				// Patch ImageConfigMap.
				imageConfigMapData := map[string]string{"CONFIDENTIAL_COMPUTE_ENABLED": "no"}
				if err := updateConfigMap(r.Client, r.Log, ig.getImageConfigMapName(), OperatorNamespace, imageConfigMapData); err != nil {
					if k8serrors.IsNotFound(err) {
						// Nothing to do, feature is disabled and configMap doesn't exist.
					} else {
						return err
					}
				}
			}
		}
	}

	// peer pods config
	if r.kataConfig.Spec.EnablePeerPods {
		// Patch peer pods configMap, if it exists.
		var peerpodsCMData map[string]string
		if state == Enabled {
			peerpodsCMData = map[string]string{"DISABLECVM": "false"}
		} else {
			peerpodsCMData = map[string]string{"DISABLECVM": "true"}
		}
		if err := updateConfigMap(r.Client, r.Log, peerpodsCMName, OperatorNamespace, peerpodsCMData); err != nil {
			if k8serrors.IsNotFound(err) {
				// When feature is Enabled: ConfigMap doesn't exist yet, will try again at the next reconcile run.
				// Else: Nothing to do, feature is disabled and configMap doesn't exist.
			} else {
				return err
			}
		}
	}

	// Handle kata-cc runtime classes for baremetal environments
	if state == Enabled {
		r.Log.Info("Creating kata-cc runtime classes for confidential containers")

		handler, nodeLabel, err := r.computeTEEHandlerAndLabel()
		if err != nil {
			r.Log.Info("TEE detection failed", "err", err)
			return fmt.Errorf("TEE detection failed: %w", err)
		}

		// Create kata-cc runtime class restricted to the detected TEE subset
		err = r.createRuntimeClass(kataCCRuntimeClassName, kataCCRuntimeClassCpuOverhead, kataCCRuntimeClassMemOverhead, handler, nodeLabel)
		if err != nil {
			r.Log.Info("Error creating kata-cc runtime class", "err", err)
			return fmt.Errorf("Error creating kata-cc runtime class: %w", err)
		}

	} else {
		r.Log.Info("Deleting kata-cc runtime classes for confidential containers")

		// Delete kata-cc runtime class
		err := r.deleteRuntimeClass(kataCCRuntimeClassName)
		if err != nil {
			r.Log.Info("Error deleting confidential container runtime classes", "err", err)
			return fmt.Errorf("Error deleting confidential container runtime classes: %w", err)
		}
	}

	return nil
}

func (r *KataConfigOpenShiftReconciler) computeTEEHandlerAndLabel() (string, string, error) {
	selector, err := r.getKataConfigNodeSelectorAsSelector()
	if err != nil {
		msg := "Couldn't build KataConfig node selector for TEE detection"
		r.Log.Info(msg, "err", err)
		return "", "", fmt.Errorf("%s: %w", msg, err)
	}

	nodes := &corev1.NodeList{}
	listOpts := []client.ListOption{
		client.MatchingLabelsSelector{Selector: selector},
	}
	if err := r.Client.List(context.TODO(), nodes, listOpts...); err != nil {
		msg := "Failed to list nodes for TEE detection"
		r.Log.Info(msg, "err", err)
		return "", "", fmt.Errorf("%s: %w", msg, err)
	}

	var hasIntelTDX bool
	var hasAmdSNP bool
	for _, n := range nodes.Items {
		if v, ok := n.Labels[intelTDXNodeLabel]; ok && v == "true" {
			hasIntelTDX = true
		}
		if v, ok := n.Labels[amdSNPNodeLabel]; ok && v == "true" {
			hasAmdSNP = true
		}
	}

	if hasIntelTDX && hasAmdSNP && !r.kataConfig.Spec.EnablePeerPods {
		msg := "Confidential install blocked: multiple TEE platforms detected in selected pool (intel TDX and AMD SNP); only one TEE per cluster is supported on baremetal"
		r.Log.Info(msg)
		return "", "", fmt.Errorf(msg)
	}

	if hasIntelTDX && !hasAmdSNP {
		return kataCCIntelHandler, intelTDXNodeLabel, nil
	}
	if hasAmdSNP && !hasIntelTDX {
		return kataCCAmdHandler, amdSNPNodeLabel, nil
	}

	msg := fmt.Sprintf("Confidential install blocked: no TEE platform labels detected (expected %s or %s) on any node in the selected pool", intelTDXNodeLabel, amdSNPNodeLabel)
	r.Log.Info(msg)
	return "", "", fmt.Errorf(msg)
}
