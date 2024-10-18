// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package kubernetes

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/retry"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	pkgApi "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

func resourceKubernetesJobV1() *schema.Resource {
	return &schema.Resource{
		Description:   "A Job creates one or more Pods and ensures that a specified number of them successfully terminate. As pods successfully complete, the Job tracks the successful completions. When a specified number of successful completions is reached, the task (ie, Job) is complete. Deleting a Job will clean up the Pods it created. A simple case is to create one Job object in order to reliably run one Pod to completion. The Job object will start a new Pod if the first Pod fails or is deleted (for example due to a node hardware failure or a node reboot. You can also use a Job to run multiple Pods in parallel. ",
		CreateContext: resourceKubernetesJobV1Create,
		ReadContext:   resourceKubernetesJobV1Read,
		UpdateContext: resourceKubernetesJobV1Update,
		DeleteContext: resourceKubernetesJobV1Delete,
		CustomizeDiff: resourceKubernetesJobV1CustomizeDiff,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
		StateUpgraders: []schema.StateUpgrader{
			{
				Version: 0,
				Type:    resourceKubernetesJobV0().CoreConfigSchema().ImpliedType(),
				Upgrade: resourceKubernetesJobUpgradeV0,
			},
		},
		SchemaVersion: 1,
		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(1 * time.Minute),
			Update: schema.DefaultTimeout(1 * time.Minute),
			Delete: schema.DefaultTimeout(1 * time.Minute),
		},
		Schema: resourceKubernetesJobV1Schema(),
	}
}

func resourceKubernetesJobV1CustomizeDiff(ctx context.Context, d *schema.ResourceDiff, meta interface{}) error {
	if d.Id() == "" {
		log.Printf("[DEBUG] Resource ID is empty, resource not created yet.")
		return nil
	}

	// Retrieve old and new TTL values as strings
	oldTTLRaw, newTTLRaw := d.GetChange("spec.0.ttl_seconds_after_finished")

	var oldTTLStr, newTTLStr string

	if oldTTLRaw != nil {
		oldTTLStr, _ = oldTTLRaw.(string)
	}
	if newTTLRaw != nil {
		newTTLStr, _ = newTTLRaw.(string)
	}

	oldTTLInt, err := strconv.Atoi(oldTTLStr)
	if err != nil {
		oldTTLInt = 0
	}
	newTTLInt, err := strconv.Atoi(newTTLStr)
	if err != nil {
		newTTLInt = 0
	}

	conn, err := meta.(KubeClientsets).MainClientset()
	if err != nil {
		return err
	}

	namespace, name, err := idParts(d.Id())
	if err != nil {
		return err
	}

	// Check if the Job exists
	_, err = conn.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Job is missing
			if oldTTLInt >= 0 {
				if oldTTLInt != newTTLInt {
					// TTL value changed; force recreation
					log.Printf("[DEBUG] Job %s not found and ttl_seconds_after_finished changed from %d to %d; forcing recreation", d.Id(), oldTTLInt, newTTLInt)
					d.ForceNew("spec.0.ttl_seconds_after_finished")
					return nil
				} else {
					// TTL remains the same; suppress diff
					log.Printf("[DEBUG] Job %s not found and ttl_seconds_after_finished remains %d; suppressing diff", d.Id(), oldTTLInt)
					d.Clear("spec")
					d.Clear("metadata")
					return nil
				}
			}
		} else {
			return err
		}
	} else {
		// Job exists, check if TTL changed
		if oldTTLInt != newTTLInt {
			// TTL changed; force recreation
			log.Printf("[DEBUG] Job %s exists and ttl_seconds_after_finished changed from %d to %d; forcing recreation", d.Id(), oldTTLInt, newTTLInt)
			d.ForceNew("spec.0.ttl_seconds_after_finished")
			return nil
		}
	}

	return nil
}

func resourceKubernetesJobV1Schema() map[string]*schema.Schema {
	return map[string]*schema.Schema{
		"metadata": jobMetadataSchema(),
		"spec": {
			Type:        schema.TypeList,
			Description: "Spec of the job owned by the cluster",
			Required:    true,
			MaxItems:    1,
			ForceNew:    false,
			Elem: &schema.Resource{
				Schema: jobSpecFields(false),
			},
		},
		"wait_for_completion": {
			Type:     schema.TypeBool,
			Optional: true,
			Default:  true,
		},
	}
}

func resourceKubernetesJobV1Create(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	conn, err := meta.(KubeClientsets).MainClientset()
	if err != nil {
		return diag.FromErr(err)
	}

	metadata := expandMetadata(d.Get("metadata").([]interface{}))
	spec, err := expandJobV1Spec(d.Get("spec").([]interface{}))
	if err != nil {
		return diag.FromErr(err)
	}

	job := batchv1.Job{
		ObjectMeta: metadata,
		Spec:       spec,
	}

	log.Printf("[INFO] Creating new Job: %#v", job)

	out, err := conn.BatchV1().Jobs(metadata.Namespace).Create(ctx, &job, metav1.CreateOptions{})
	if err != nil {
		return diag.Errorf("Failed to create Job! API error: %s", err)
	}
	log.Printf("[INFO] Submitted new job: %#v", out)

	d.SetId(buildId(out.ObjectMeta))

	namespace, name, err := idParts(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}
	if d.Get("wait_for_completion").(bool) {
		err = retry.RetryContext(ctx, d.Timeout(schema.TimeoutCreate),
			retryUntilJobV1IsFinished(ctx, conn, namespace, name))
		if err != nil {
			return diag.FromErr(err)
		}
		return diag.Diagnostics{}
	}

	return resourceKubernetesJobV1Read(ctx, d, meta)
}

func resourceKubernetesJobV1Read(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	exists, err := resourceKubernetesJobV1Exists(ctx, d, meta)
	if err != nil {
		return diag.FromErr(err)
	}
	if !exists {
		// Check if ttl_seconds_after_finished is set
		if ttl, ok := d.GetOk("spec.0.ttl_seconds_after_finished"); ok {
			// ttl_seconds_after_finished is set, Job is deleted due to TTL
			// We don't need to remove the resource from the state
			log.Printf("[INFO] Job %s has been deleted by Kubernetes due to TTL (ttl_seconds_after_finished = %v), keeping resource in state", d.Id(), ttl)
			return diag.Diagnostics{}
		} else {
			// ttl_seconds_after_finished is not set, remove the resource from the state
			d.SetId("")
			return diag.Diagnostics{}
		}
	}
	conn, err := meta.(KubeClientsets).MainClientset()
	if err != nil {
		return diag.FromErr(err)
	}

	namespace, name, err := idParts(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}

	log.Printf("[INFO] Reading job %s", name)
	job, err := conn.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		log.Printf("[DEBUG] Received error: %#v", err)
		return diag.Errorf("Failed to read Job! API error: %s", err)
	}
	log.Printf("[INFO] Received job: %#v", job)

	// Remove server-generated labels unless using manual selector
	if _, ok := d.GetOk("spec.0.manual_selector"); !ok {
		removeGeneratedLabels(job.ObjectMeta.Labels)
		removeGeneratedLabels(job.Spec.Selector.MatchLabels)
	}

	err = d.Set("metadata", flattenMetadata(job.ObjectMeta, d, meta))
	if err != nil {
		return diag.FromErr(err)
	}

	jobSpec, err := flattenJobV1Spec(job.Spec, d, meta)
	if err != nil {
		return diag.FromErr(err)
	}

	err = d.Set("spec", jobSpec)
	if err != nil {
		return diag.FromErr(err)
	}
	return diag.Diagnostics{}
}

func resourceKubernetesJobV1Update(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	conn, err := meta.(KubeClientsets).MainClientset()
	if err != nil {
		return diag.FromErr(err)
	}

	namespace, name, err := idParts(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}

	// Attempt to get the Job
	_, err = conn.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Job is missing; check TTL
			ttlAttr := d.Get("spec.0.ttl_seconds_after_finished")
			ttlStr, _ := ttlAttr.(string)
			ttlInt, err := strconv.Atoi(ttlStr)
			if err != nil {
				ttlInt = 0
			}

			if ttlInt >= 0 {
				// Job was deleted due to TTL nothing to update
				log.Printf("[INFO] Job %s not found but ttl_seconds_after_finished = %v; nothing to update", d.Id(), ttlInt)
				return nil
			}

			// Job was deleted unexpectedly; return an error
			return diag.Errorf("Job %s not found; cannot update because it has been deleted", d.Id())
		}
		return diag.Errorf("Error retrieving Job: %s", err)
	}

	// Proceed with the update as usual
	ops := patchMetadata("metadata.0.", "/metadata/", d)

	if d.HasChange("spec") {
		specOps := patchJobV1Spec("/spec", "spec.0.", d)
		ops = append(ops, specOps...)
	}

	data, err := ops.MarshalJSON()
	if err != nil {
		return diag.Errorf("Failed to marshal update operations: %s", err)
	}

	log.Printf("[INFO] Updating job %s: %#v", d.Id(), ops)

	out, err := conn.BatchV1().Jobs(namespace).Patch(ctx, name, pkgApi.JSONPatchType, data, metav1.PatchOptions{})
	if err != nil {
		return diag.Errorf("Failed to update Job! API error: %s", err)
	}
	log.Printf("[INFO] Submitted updated job: %#v", out)

	d.SetId(buildId(out.ObjectMeta))

	if d.Get("wait_for_completion").(bool) {
		err = retry.RetryContext(ctx, d.Timeout(schema.TimeoutUpdate),
			retryUntilJobV1IsFinished(ctx, conn, namespace, name))
		if err != nil {
			return diag.FromErr(err)
		}
	}
	return resourceKubernetesJobV1Read(ctx, d, meta)
}
func resourceKubernetesJobV1Delete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	conn, err := meta.(KubeClientsets).MainClientset()
	if err != nil {
		return diag.FromErr(err)
	}

	namespace, name, err := idParts(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}

	log.Printf("[INFO] Deleting job: %#v", name)
	err = conn.BatchV1().Jobs(namespace).Delete(ctx, name, deleteOptions)
	if err != nil {
		if statusErr, ok := err.(*errors.StatusError); ok && errors.IsNotFound(statusErr) {
			return nil
		}
		return diag.Errorf("Failed to delete Job! API error: %s", err)
	}

	err = retry.RetryContext(ctx, d.Timeout(schema.TimeoutDelete), func() *retry.RetryError {
		_, err := conn.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if statusErr, ok := err.(*errors.StatusError); ok && errors.IsNotFound(statusErr) {
				return nil
			}
			return retry.NonRetryableError(err)
		}

		e := fmt.Errorf("Job %s still exists", name)
		return retry.RetryableError(e)
	})
	if err != nil {
		return diag.FromErr(err)
	}

	log.Printf("[INFO] Job %s deleted", name)

	d.SetId("")
	return nil
}

func resourceKubernetesJobV1Exists(ctx context.Context, d *schema.ResourceData, meta interface{}) (bool, error) {
	conn, err := meta.(KubeClientsets).MainClientset()
	if err != nil {
		return false, err
	}

	namespace, name, err := idParts(d.Id())
	if err != nil {
		return false, err
	}

	log.Printf("[INFO] Checking job %s", name)
	_, err = conn.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if statusErr, ok := err.(*errors.StatusError); ok && errors.IsNotFound(statusErr) {
			return false, nil
		}
		log.Printf("[DEBUG] Received error: %#v", err)
	}
	return true, err
}

// retryUntilJobV1IsFinished checks if a given job has finished its execution in either a Complete or Failed state
func retryUntilJobV1IsFinished(ctx context.Context, conn *kubernetes.Clientset, ns, name string) retry.RetryFunc {
	return func() *retry.RetryError {
		job, err := conn.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if statusErr, ok := err.(*errors.StatusError); ok && errors.IsNotFound(statusErr) {
				return nil
			}
			return retry.NonRetryableError(err)
		}

		for _, c := range job.Status.Conditions {
			if c.Status == corev1.ConditionTrue {
				log.Printf("[DEBUG] Current condition of job: %s/%s: %s\n", ns, name, c.Type)
				switch c.Type {
				case batchv1.JobComplete:
					return nil
				case batchv1.JobFailed:
					return retry.NonRetryableError(fmt.Errorf("job: %s/%s is in failed state", ns, name))
				}
			}
		}

		return retry.RetryableError(fmt.Errorf("job: %s/%s is not in complete state", ns, name))
	}
}
