package atlasproject

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mongodb/mongodb-atlas-kubernetes/pkg/util/toptr"

	"go.mongodb.org/atlas/mongodbatlas"

	"github.com/mongodb/mongodb-atlas-kubernetes/pkg/controller/customresource"

	mdbv1 "github.com/mongodb/mongodb-atlas-kubernetes/pkg/api/v1"
	"github.com/mongodb/mongodb-atlas-kubernetes/pkg/api/v1/project"
	"github.com/mongodb/mongodb-atlas-kubernetes/pkg/api/v1/status"
	"github.com/mongodb/mongodb-atlas-kubernetes/pkg/controller/workflow"
)

// ensureMaintenanceWindow ensures that the state of the Atlas Maintenance Window matches the
// state of the Maintenance Window specified in the project CR. If a Maintenance Window exists
// in Atlas but is not specified in the CR, it is deleted.
func ensureMaintenanceWindow(ctx context.Context, workflowCtx *workflow.Context, atlasProject *mdbv1.AtlasProject, protected bool) workflow.Result {
	canReconcile, err := canMaintenanceWindowReconcile(ctx, workflowCtx.Client, protected, atlasProject)
	if err != nil {
		result := workflow.Terminate(workflow.Internal, fmt.Sprintf("unable to resolve ownership for deletion protection: %s", err))
		workflowCtx.SetConditionFromResult(status.IPAccessListReadyType, result)

		return result
	}

	if !canReconcile {
		result := workflow.Terminate(
			workflow.AtlasDeletionProtection,
			"unable to reconcile Maintenance Window due to deletion protection being enabled. see https://dochub.mongodb.org/core/ako-deletion-protection for further information",
		)
		workflowCtx.SetConditionFromResult(status.MaintenanceWindowReadyType, result)

		return result
	}

	if isEmptyWindow(atlasProject.Spec.MaintenanceWindow) {
		if condition, found := workflowCtx.GetCondition(status.MaintenanceWindowReadyType); found {
			workflowCtx.Log.Debugw("Window is empty, deleting in Atlas")
			if result := deleteInAtlas(workflowCtx.Client, atlasProject.ID()); !result.IsOk() {
				workflowCtx.SetConditionFromResult(condition.Type, result)
				return result
			}
			workflowCtx.UnsetCondition(condition.Type)
		}

		return workflow.OK()
	}

	if result := syncAtlasWithSpec(workflowCtx, atlasProject.ID(), atlasProject.Spec.MaintenanceWindow); !result.IsOk() {
		workflowCtx.SetConditionFromResult(status.MaintenanceWindowReadyType, result)
		return result
	}

	workflowCtx.SetConditionTrue(status.MaintenanceWindowReadyType)
	return workflow.OK()
}

func syncAtlasWithSpec(ctx *workflow.Context, projectID string, windowSpec project.MaintenanceWindow) workflow.Result {
	ctx.Log.Debugw("Validate the maintenance window")
	if err := validateMaintenanceWindow(windowSpec); err != nil {
		return workflow.Terminate(workflow.ProjectWindowInvalid, err.Error())
	}

	ctx.Log.Debugw("Checking if window needs update")
	windowInAtlas, result := getInAtlas(ctx.Client, projectID)
	if !result.IsOk() {
		return result
	}

	if daysOrHoursAreDifferent(windowInAtlas, windowSpec) {
		ctx.Log.Debugw("Creating or updating window")
		// We set startASAP to false because the operator takes care of calling the API a second time if both
		// startASAP and the new maintenance timeslots are defined
		if result := createOrUpdateInAtlas(ctx.Client, projectID, windowSpec.WithStartASAP(false)); !result.IsOk() {
			return result
		}
	} else if *windowInAtlas.AutoDeferOnceEnabled != windowSpec.AutoDefer {
		// If autoDefer flag is different in Atlas, and we haven't updated the window previously, we toggle the flag
		ctx.Log.Debugw("Toggling autoDefer")
		if result := toggleAutoDeferInAtlas(ctx.Client, projectID); !result.IsOk() {
			return result
		}
	}

	if windowSpec.StartASAP {
		ctx.Log.Debugw("Starting maintenance ASAP")
		// To avoid any unexpected behavior, we send a request to the API containing only the StartASAP flag,
		// although the API should ignore other fields in that case
		if result := createOrUpdateInAtlas(ctx.Client, projectID, project.NewMaintenanceWindow().WithStartASAP(true)); !result.IsOk() {
			return result
		}
		// Nothing else should be done after sending a StartASAP request
		return workflow.OK()
	}

	if windowSpec.Defer {
		ctx.Log.Debugw("Deferring scheduled maintenance")
		if result := deferInAtlas(ctx.Client, projectID); !result.IsOk() {
			return result
		}
		// Nothing else should be done after deferring
		return workflow.OK()
	}

	return workflow.OK()
}

func isEmpty(i int) bool {
	return i == 0
}

func isEmptyWindow(window project.MaintenanceWindow) bool {
	return isEmpty(window.DayOfWeek) && isEmpty(window.HourOfDay) && !window.StartASAP && !window.Defer && !window.AutoDefer
}

func windowSpecified(window project.MaintenanceWindow) bool {
	return !isEmpty(window.DayOfWeek)
}

func maxOneFlag(window project.MaintenanceWindow) bool {
	return !(window.StartASAP && window.Defer)
}

func daysOrHoursAreDifferent(windowInAtlas *mongodbatlas.MaintenanceWindow, windowSpec project.MaintenanceWindow) bool {
	return windowInAtlas.DayOfWeek == 0 || windowInAtlas.HourOfDay == nil ||
		*windowInAtlas.HourOfDay != windowSpec.HourOfDay || windowInAtlas.DayOfWeek != windowSpec.DayOfWeek
}

// validateMaintenanceWindow performs validation of the Maintenance Window. Note, that we intentionally don't validate
// that hour of day and day of week are in the bounds - this will be done by Atlas.
func validateMaintenanceWindow(window project.MaintenanceWindow) error {
	if windowSpecified(window) && maxOneFlag(window) {
		return nil
	}
	errorString := "projectMaintenanceWindow must respect the following constraints, or be empty : " +
		"1) dayOfWeek must be specified (hourOfDay is 0 by default, autoDeferral is false by default) " +
		"2) only one of (startASAP, defer) is true"
	return errors.New(errorString)
}

// operatorToAtlasMaintenanceWindow converts the maintenanceWindow specified in the project CR to the format
// expected by the Atlas API.
func operatorToAtlasMaintenanceWindow(maintenanceWindow project.MaintenanceWindow) (*mongodbatlas.MaintenanceWindow, workflow.Result) {
	operatorWindow := maintenanceWindow.ToAtlas()
	return operatorWindow, workflow.OK()
}

func getInAtlas(client mongodbatlas.Client, projectID string) (*mongodbatlas.MaintenanceWindow, workflow.Result) {
	window, _, err := client.MaintenanceWindows.Get(context.Background(), projectID)
	if err != nil {
		return nil, workflow.Terminate(workflow.ProjectWindowNotObtainedFromAtlas, err.Error())
	}
	return window, workflow.OK()
}

func createOrUpdateInAtlas(client mongodbatlas.Client, projectID string, maintenanceWindow project.MaintenanceWindow) workflow.Result {
	operatorWindow, status := operatorToAtlasMaintenanceWindow(maintenanceWindow)
	if !status.IsOk() {
		return status
	}

	if _, err := client.MaintenanceWindows.Update(context.Background(), projectID, operatorWindow); err != nil {
		return workflow.Terminate(workflow.ProjectWindowNotCreatedInAtlas, err.Error())
	}
	return workflow.OK()
}

func deleteInAtlas(client mongodbatlas.Client, projectID string) workflow.Result {
	if _, err := client.MaintenanceWindows.Reset(context.Background(), projectID); err != nil {
		return workflow.Terminate(workflow.ProjectWindowNotDeletedInAtlas, err.Error())
	}
	return workflow.OK()
}

func deferInAtlas(client mongodbatlas.Client, projectID string) workflow.Result {
	if _, err := client.MaintenanceWindows.Defer(context.Background(), projectID); err != nil {
		return workflow.Terminate(workflow.ProjectWindowNotDeferredInAtlas, err.Error())
	}
	return workflow.OK()
}

// toggleAutoDeferInAtlas toggles the field "autoDeferOnceEnabled" by sending a POST /autoDefer request to the API
func toggleAutoDeferInAtlas(client mongodbatlas.Client, projectID string) workflow.Result {
	if _, err := client.MaintenanceWindows.AutoDefer(context.Background(), projectID); err != nil {
		return workflow.Terminate(workflow.ProjectWindowNotAutoDeferredInAtlas, err.Error())
	}
	return workflow.OK()
}

func canMaintenanceWindowReconcile(ctx context.Context, atlasClient mongodbatlas.Client, protected bool, akoProject *mdbv1.AtlasProject) (bool, error) {
	if !protected {
		return true, nil
	}

	latestConfig := &mdbv1.AtlasProjectSpec{}
	latestConfigString, ok := akoProject.Annotations[customresource.AnnotationLastAppliedConfiguration]
	if ok {
		if err := json.Unmarshal([]byte(latestConfigString), latestConfig); err != nil {
			return false, err
		}
	}

	mWindow, _, err := atlasClient.MaintenanceWindows.Get(ctx, akoProject.ID())
	if err != nil {
		return false, err
	}

	if isAtlasMaintenanceWindowEmpty(mWindow) {
		return true, nil
	}

	return isMaintenanceWindowConfigEqual(latestConfig.MaintenanceWindow, *mWindow) ||
		isMaintenanceWindowConfigEqual(akoProject.Spec.MaintenanceWindow, *mWindow), nil
}

func isMaintenanceWindowConfigEqual(akoMWindow project.MaintenanceWindow, atlasMWindow mongodbatlas.MaintenanceWindow) bool {
	if atlasMWindow.HourOfDay == nil {
		atlasMWindow.HourOfDay = toptr.MakePtr(0)
	}

	if atlasMWindow.StartASAP == nil {
		atlasMWindow.StartASAP = toptr.MakePtr(false)
	}

	if atlasMWindow.AutoDeferOnceEnabled == nil {
		atlasMWindow.AutoDeferOnceEnabled = toptr.MakePtr(false)
	}

	return akoMWindow.DayOfWeek == atlasMWindow.DayOfWeek &&
		akoMWindow.HourOfDay == *atlasMWindow.HourOfDay &&
		akoMWindow.StartASAP == *atlasMWindow.StartASAP &&
		akoMWindow.AutoDefer == *atlasMWindow.AutoDeferOnceEnabled
}

func isAtlasMaintenanceWindowEmpty(mWindow *mongodbatlas.MaintenanceWindow) bool {
	return mWindow.DayOfWeek == 0 && mWindow.HourOfDay == nil && mWindow.StartASAP == nil && mWindow.AutoDeferOnceEnabled == nil
}
