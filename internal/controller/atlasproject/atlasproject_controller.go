// Copyright 2020 MongoDB Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package atlasproject

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/mongodb/mongodb-atlas-kubernetes/v2/api"
	akov2 "github.com/mongodb/mongodb-atlas-kubernetes/v2/api/v1"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/controller/atlas"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/controller/customresource"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/controller/reconciler"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/controller/statushandler"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/controller/validate"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/controller/workflow"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/indexer"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/pointer"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/translation/encryptionatrest"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/translation/maintenancewindow"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/translation/project"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/translation/teams"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/ratelimit"
)

// AtlasProjectReconciler reconciles a AtlasProject object
type AtlasProjectReconciler struct {
	Client                      client.Client
	Log                         *zap.SugaredLogger
	Scheme                      *runtime.Scheme
	GlobalPredicates            []predicate.Predicate
	EventRecorder               record.EventRecorder
	AtlasProvider               atlas.Provider
	ObjectDeletionProtection    bool
	SubObjectDeletionProtection bool
	GlobalSecretRef             client.ObjectKey
}

type AtlasProjectServices struct {
	projectService          project.ProjectService
	teamsService            teams.TeamsService
	maintenanceService      maintenancewindow.MaintenanceWindowService
	encryptionAtRestService encryptionatrest.EncryptionAtRestService
}

// Dev note: duplicate the permissions in both sections below to generate both Role and ClusterRoles

// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlasprojects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlasprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlasprojects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlasprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",namespace=default,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",namespace=default,resources=events,verbs=create;patch

// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlasteams,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlasteams/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlasteams,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlasteams/status,verbs=get;update;patch

func (r *AtlasProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.With("atlasproject", req.NamespacedName)

	atlasProject := &akov2.AtlasProject{}
	result := customresource.PrepareResource(ctx, r.Client, req, atlasProject, log)
	if !result.IsOk() {
		return result.ReconcileResult()
	}

	if customresource.ReconciliationShouldBeSkipped(atlasProject) {
		log.Infow(fmt.Sprintf("-> Skipping AtlasProject reconciliation as annotation %s=%s", customresource.ReconciliationPolicyAnnotation, customresource.ReconciliationPolicySkip), "spec", atlasProject.Spec)
		if !atlasProject.GetDeletionTimestamp().IsZero() {
			err := customresource.ManageFinalizer(ctx, r.Client, atlasProject, customresource.UnsetFinalizer)
			if err != nil {
				result = workflow.Terminate(workflow.Internal, err)
				log.Errorw("Failed to remove finalizer", "error", err)
				return result.ReconcileResult()
			}
		}

		if err := r.clearLastAppliedMigratedResources(ctx, atlasProject); err != nil {
			result = workflow.Terminate(workflow.Internal, err)
			log.Errorw("Failed to clear migrated independent resources", "error", err)
			return result.ReconcileResult()
		}

		return workflow.OK().ReconcileResult()
	}

	conditions := akov2.InitCondition(atlasProject, api.FalseCondition(api.ReadyType))
	workflowCtx := workflow.NewContext(log, conditions, ctx, atlasProject)
	log.Infow("-> Starting AtlasProject reconciliation", "spec", atlasProject.Spec)

	// This update will make sure the status is always updated in case of any errors or successful result
	defer func() {
		statushandler.Update(workflowCtx, r.Client, r.EventRecorder, atlasProject)
	}()

	resourceVersionIsValid := customresource.ValidateResourceVersion(workflowCtx, atlasProject, r.Log)
	if !resourceVersionIsValid.IsOk() {
		r.Log.Debugf("project validation result: %v", resourceVersionIsValid)
		return resourceVersionIsValid.ReconcileResult()
	}

	if err := validate.Project(atlasProject, r.AtlasProvider.IsCloudGov()); err != nil {
		result := workflow.Terminate(workflow.Internal, err)
		setCondition(workflowCtx, api.ValidationSucceeded, result)
		return result.ReconcileResult()
	}
	workflowCtx.SetConditionTrue(api.ValidationSucceeded)

	if !r.AtlasProvider.IsResourceSupported(atlasProject) {
		result := workflow.Terminate(workflow.AtlasGovUnsupported, errors.New("the AtlasProject is not supported by Atlas for government")).
			WithoutRetry()
		setCondition(workflowCtx, api.ProjectReadyType, result)
		return result.ReconcileResult()
	}

	connectionConfig, err := reconciler.GetConnectionConfig(ctx, r.Client, atlasProject.ConnectionSecretObjectKey(), &r.GlobalSecretRef)
	if err != nil {
		result := workflow.Terminate(workflow.AtlasAPIAccessNotConfigured, err)
		setCondition(workflowCtx, api.ProjectReadyType, result)
		return result.ReconcileResult()
	}

	atlasSdkClient, err := r.AtlasProvider.SdkClientSet(ctx, connectionConfig.Credentials, log)
	if err != nil {
		result := workflow.Terminate(workflow.AtlasAPIAccessNotConfigured, err)
		setCondition(workflowCtx, api.ProjectReadyType, result)
		return result.ReconcileResult()
	}

	workflowCtx.SdkClientSet = atlasSdkClient
	services := AtlasProjectServices{}
	services.projectService = project.NewProjectAPIService(atlasSdkClient.SdkClient20250312002.ProjectsApi)
	services.teamsService = teams.NewTeamsAPIService(atlasSdkClient.SdkClient20250312002.TeamsApi, atlasSdkClient.SdkClient20250312002.MongoDBCloudUsersApi)
	services.maintenanceService = maintenancewindow.NewMaintenanceWindowAPIService(atlasSdkClient.SdkClient20250312002.MaintenanceWindowsApi)
	services.encryptionAtRestService = encryptionatrest.NewEncryptionAtRestAPI(atlasSdkClient.SdkClient20250312002.EncryptionAtRestUsingCustomerKeyManagementApi)

	atlasClient, err := r.AtlasProvider.Client(workflowCtx.Context, connectionConfig.Credentials, log)
	if err != nil {
		result := workflow.Terminate(workflow.AtlasAPIAccessNotConfigured, err)
		setCondition(workflowCtx, api.ProjectReadyType, result)
		return result.ReconcileResult()
	}
	workflowCtx.OrgID = connectionConfig.OrgID
	workflowCtx.Client = atlasClient

	return r.handleProject(workflowCtx, connectionConfig.OrgID, atlasProject, &services)
}

// ensureProjectResources ensures IP Access List, Private Endpoints, Integrations, Maintenance Window and Encryption at Rest
func (r *AtlasProjectReconciler) ensureProjectResources(workflowCtx *workflow.Context, project *akov2.AtlasProject, services *AtlasProjectServices) (results []workflow.DeprecatedResult) {
	for k, v := range project.Annotations {
		workflowCtx.Log.Debugf(k)
		workflowCtx.Log.Debugf(v)
	}

	var result workflow.DeprecatedResult
	if result = handleIPAccessList(workflowCtx, project); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.IPAccessListReadyType), "")
	}
	results = append(results, result)

	if result = ensurePrivateEndpoint(workflowCtx, project); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.PrivateEndpointReadyType), "")
	}
	results = append(results, result)

	if result = ensureCloudProviderIntegration(workflowCtx, project); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.CloudProviderIntegrationReadyType), "")
	}
	results = append(results, result)

	if result = ensureNetworkPeers(workflowCtx, project); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.NetworkPeerReadyType), "")
	}
	results = append(results, result)

	if result = r.ensureAlertConfigurations(workflowCtx, project); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.AlertConfigurationReadyType), "")
	}
	results = append(results, result)

	if result = r.ensureIntegration(workflowCtx, project); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.IntegrationReadyType), "")
	}
	results = append(results, result)

	if result = r.ensureMaintenanceWindow(workflowCtx, project, services.maintenanceService); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.MaintenanceWindowReadyType), "")
	}
	results = append(results, result)

	if result = r.ensureEncryptionAtRest(workflowCtx, project, services.encryptionAtRestService); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.EncryptionAtRestReadyType), "")
	}
	results = append(results, result)

	if result = handleAudit(workflowCtx, project); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.AuditingReadyType), "")
	}
	results = append(results, result)

	if result = ensureProjectSettings(workflowCtx, project); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.ProjectSettingsReadyType), "")
	}
	results = append(results, result)

	if result = ensureCustomRoles(workflowCtx, project); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.ProjectCustomRolesReadyType), "")
	}
	results = append(results, result)

	if result = r.ensureAssignedTeams(workflowCtx, services.teamsService, project); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.ProjectTeamsReadyType), "")
	}
	results = append(results, result)

	if result = r.ensureBackupCompliance(workflowCtx, project); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.BackupComplianceReadyType), "")
	}
	results = append(results, result)

	if result = r.ensureX509(workflowCtx, project); result.IsOk() {
		r.EventRecorder.Event(project, "Normal", string(api.X509AuthReadyType), "")
	}
	results = append(results, result)

	return results
}

func (r *AtlasProjectReconciler) For() (client.Object, builder.Predicates) {
	return &akov2.AtlasProject{}, builder.WithPredicates(r.GlobalPredicates...)
}

func (r *AtlasProjectReconciler) SetupWithManager(mgr ctrl.Manager, skipNameValidation bool) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("AtlasProject").
		For(r.For()).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(newProjectsMapFunc[corev1.Secret](indexer.AtlasProjectBySecretsIndex, r.Client, r.Log)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&akov2.AtlasTeam{},
			handler.EnqueueRequestsFromMapFunc(newProjectsMapFunc[akov2.AtlasTeam](indexer.AtlasProjectByTeamIndex, r.Client, r.Log)),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&akov2.AtlasBackupCompliancePolicy{},
			handler.EnqueueRequestsFromMapFunc(newProjectsMapFunc[akov2.AtlasBackupCompliancePolicy](indexer.AtlasProjectByBackupCompliancePolicyIndex, r.Client, r.Log)),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		WithOptions(controller.TypedOptions[reconcile.Request]{
			RateLimiter:        ratelimit.NewRateLimiter[reconcile.Request](),
			SkipNameValidation: pointer.MakePtr(skipNameValidation)}).
		Complete(r)
}

func NewAtlasProjectReconciler(
	c cluster.Cluster,
	predicates []predicate.Predicate,
	atlasProvider atlas.Provider,
	deletionProtection bool,
	logger *zap.Logger,
	globalSecretRef client.ObjectKey,
) *AtlasProjectReconciler {
	return &AtlasProjectReconciler{
		Scheme:                   c.GetScheme(),
		Client:                   c.GetClient(),
		EventRecorder:            c.GetEventRecorderFor("AtlasProject"),
		GlobalPredicates:         predicates,
		Log:                      logger.Named("controllers").Named("AtlasProject").Sugar(),
		AtlasProvider:            atlasProvider,
		ObjectDeletionProtection: deletionProtection,
		GlobalSecretRef:          globalSecretRef,
	}
}

func newProjectsMapFunc[T any](indexName string, kubeClient client.Client, logger *zap.SugaredLogger) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		_, ok := any(obj).(*T)
		if !ok {
			var watchedObject T
			logger.Warnf("watching %T but got %T", &watchedObject, obj)
			return nil
		}

		projects := &akov2.AtlasProjectList{}
		listOpts := &client.ListOptions{
			FieldSelector: fields.OneTermEqualSelector(
				indexName,
				client.ObjectKeyFromObject(obj).String(),
			),
		}
		err := kubeClient.List(ctx, projects, listOpts)
		if err != nil {
			logger.Errorf("failed to list Atlas projects: %e", err)
			return []reconcile.Request{}
		}

		requests := make([]reconcile.Request, 0, len(projects.Items))
		for i := range projects.Items {
			item := projects.Items[i]
			requests = append(
				requests,
				reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      item.Name,
						Namespace: item.Namespace,
					},
				},
			)
		}
		return requests
	}
}

// setCondition sets the condition from the result and logs the warnings
func setCondition(ctx *workflow.Context, condition api.ConditionType, result workflow.DeprecatedResult) {
	ctx.SetConditionFromResult(condition, result)
	logIfWarning(ctx, result)
}

func logIfWarning(ctx *workflow.Context, result workflow.DeprecatedResult) {
	if result.IsWarning() {
		ctx.Log.Warnw(result.GetMessage())
	}
}

func lastAppliedSpecFrom(atlasProject *akov2.AtlasProject) (*akov2.AtlasProjectSpec, error) {
	var lastApplied akov2.AtlasProject
	ann, ok := atlasProject.GetAnnotations()[customresource.AnnotationLastAppliedConfiguration]

	if !ok {
		return nil, nil
	}

	err := json.Unmarshal([]byte(ann), &lastApplied.Spec)
	if err != nil {
		return nil, fmt.Errorf("error reading AtlasProject Spec from annotation [%s]: %w",
			customresource.AnnotationLastAppliedConfiguration, err)
	}

	return &lastApplied.Spec, nil
}

func (r *AtlasProjectReconciler) clearLastAppliedMigratedResources(ctx context.Context, atlasProject *akov2.AtlasProject) error {
	clearedCfg, err := customresource.ParseLastConfigApplied[akov2.AtlasProjectSpec](atlasProject)
	if err != nil {
		return fmt.Errorf("failed to parse last applied config annotation: %w", err)
	}
	if clearedCfg == nil { // nothing to patch
		return nil
	}
	// clear all resources migrated as independent CRDs to avoid eager
	// reconciliation that might conflict with independent CRs and apply
	clearedCfg.CustomRoles = nil
	clearedCfg.PrivateEndpoints = nil
	clearedCfg.ProjectIPAccessList = nil
	clearedCfg.NetworkPeers = nil
	clearedCfg.Integrations = nil

	if err := customresource.PatchLastConfigApplied(ctx, r.Client, atlasProject, clearedCfg); err != nil {
		return fmt.Errorf("failed to clear migrated resources in last applied config annotation: %w", err)
	}
	return nil
}
