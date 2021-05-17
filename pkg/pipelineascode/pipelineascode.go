package pipelineascode

import (
	"context"
	"fmt"
	"path/filepath"

	apipac "github.com/openshift-pipelines/pipelines-as-code/pkg/apis/pipelinesascode/v1alpha1"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/cli"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/config"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/resolve"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/webvcs"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	tektonDir               = ".tekton"
	tektonConfigurationFile = "tekton.yaml"
	maxPipelineRunStatusRun = 5
)

type Options struct {
	Payload     string
	PayloadFile string
	RunInfo     webvcs.RunInfo
}

func getRepoByCR(ctx context.Context, cs *cli.Clients, url, branch, forceNamespace string) (apipac.Repository, error) {
	var repository apipac.Repository

	repositories, err := cs.PipelineAsCode.PipelinesascodeV1alpha1().Repositories("").List(
		ctx, metav1.ListOptions{})
	if err != nil {
		return repository, err
	}
	for _, value := range repositories.Items {
		if value.Spec.URL == url && value.Spec.Branch == branch {
			if forceNamespace != "" && value.Namespace != forceNamespace {
				return repository, fmt.Errorf(
					"repo CR matches but should be installed in %s as configured from tekton.yaml on the main branch",
					forceNamespace)
			}

			// Disallow attempts for hijacks. If the installed CR is not configured on the
			// Namespace the Spec is targeting then disallow it.
			if value.Namespace != value.Spec.Namespace {
				return repository, fmt.Errorf("repo CR %s matches but belongs to %s while it should be in %s",
					value.Name,
					value.Namespace,
					value.Spec.Namespace)
			}
			return value, nil
		}
	}
	return repository, nil
}

// Run over the main loop
func Run(ctx context.Context, cs *cli.Clients, k8int cli.KubeInteractionIntf, runinfo *webvcs.RunInfo) error {
	var err error
	var maintekton TektonYamlConfig

	// Create first check run to let know the user we have started the pipeline
	checkRun, err := cs.GithubClient.CreateCheckRun(ctx, "in_progress", runinfo)
	if err != nil {
		return err
	}
	// Set the runId on runInfo so if we have an error we can report it on UI (GH checks UI for GH PR)
	runinfo.CheckRunID = checkRun.ID

	// Check if submitted is allowed to run this.
	allowed, err := aclCheck(ctx, cs, runinfo)
	if err != nil {
		return err
	}
	if !allowed {
		_, _ = cs.GithubClient.CreateStatus(ctx, runinfo, "completed", "skipped",
			fmt.Sprintf("User %s is not allowed to run CI on this repo.", runinfo.Sender),
			"https://tenor.com/search/police-cat-gifs")
		cs.Log.Infof("User %s is not allowed to run CI on this repo", runinfo.Sender)
		return nil
	}

	// Checkout the tekton.yaml from the main/default branch, we get configuration for there.
	// TODO: to trash away
	maintektonyaml, _ := cs.GithubClient.GetFileFromDefaultBranch(ctx, filepath.Join(tektonDir, tektonConfigurationFile), runinfo)
	if maintektonyaml != "" {
		maintekton, err = processTektonYaml(ctx, cs, runinfo, maintektonyaml)
		if err != nil {
			return err
		}
	}

	// Match the Event to a Repository Resource
	repo, err := getRepoByCR(ctx, cs, runinfo.URL, runinfo.BaseBranch, maintekton.Namespace)
	if err != nil {
		return err
	}
	if repo.Spec.Namespace == "" {
		_, _ = cs.GithubClient.CreateStatus(ctx, runinfo, "completed", "skipped",
			"Could not find a configuration for this repository", "https://tenor.com/search/sad-cat-gifs")
		cs.Log.Infof("Could not find a namespace match for %s/%s on %s", runinfo.Owner, runinfo.Repository, runinfo.BaseBranch)
		return nil
	}

	// Get everything in tekton directory
	objects, err := cs.GithubClient.GetTektonDir(ctx, tektonDir, runinfo)
	if err != nil {
		_, _ = cs.GithubClient.CreateStatus(ctx, runinfo, "completed", "skipped",
			"😿 Could not find a <b>.tekton/</b> directory for this repository", "https://tenor.com/search/sad-cat-gifs")
		return err
	}
	cs.Log.Infow("Loading payload",
		"url", runinfo.URL,
		"branch", runinfo.BaseBranch,
		"sha", runinfo.SHA,
		"event_type", "pull_request")

	// Make sure we have the namespace already created or error it.
	// TODO: this probably can be trashed since repo is only can be created in
	// Namespace
	err = k8int.GetNamespace(ctx, repo.Spec.Namespace)
	if err != nil {
		return err
	}

	// Update status in UI
	_, err = cs.GithubClient.CreateStatus(ctx, runinfo, "in_progress", "",
		fmt.Sprintf("Getting pipelinerun configuration in namespace <b>%s</b>", repo.Spec.Namespace),
		"https://tenor.com/search/sad-cat-gifs")
	if err != nil {
		return err
	}

	// Process the tekton.yaml, we will get the extra tasks from there
	// TODO: to trash when we get tasks inside annotations imp
	var yamlConfig TektonYamlConfig
	for _, file := range objects {
		if file.GetName() == tektonConfigurationFile {
			data, err := cs.GithubClient.GetObject(ctx, file.GetSHA(), runinfo)
			if err != nil {
				return err
			}

			yamlConfig, err = processTektonYaml(ctx, cs, runinfo, string(data))
			if err != nil {
				return err
			}
			break
		}
	}

	// Concat all yaml files as one multi document yaml string
	allTemplates, err := cs.GithubClient.ConcatAllYamlFiles(ctx, objects, runinfo)
	if err != nil {
		return err
	}

	// Replace those {{var}} placeholders user has in her template to the runinfo variable
	allTemplates = ReplacePlaceHoldersVariables(allTemplates, map[string]string{
		"revision": runinfo.SHA,
		"repo_url": runinfo.URL,
	})

	// Append the remote task to the big template, we don't do replacement on those, maybe we should?
	if yamlConfig.RemoteTasks != "" {
		allTemplates += yamlConfig.RemoteTasks
	}

	// Merge everything (i.e: tasks/pipeline etc..) as a single pipelinerun
	pipelineRuns, err := resolve.Resolve(cs, allTemplates, true)
	if err != nil {
		return err
	}

	// Match the pipelinerun with annotation
	pipelineRun, err := config.MatchPipelinerunByAnnotation(pipelineRuns, cs, runinfo)
	if err != nil {
		return err
	}

	// Add labels on the soon to be created pipelinerun so UI/CLI can easily
	// query them.
	pipelineRun.Labels = map[string]string{
		"tekton.dev/pipeline-ascode-owner":      runinfo.Owner,
		"tekton.dev/pipeline-ascode-repository": runinfo.Repository,
		"tekton.dev/pipeline-ascode-sha":        runinfo.SHA,
		"tekton.dev/pipeline-ascode-sender":     runinfo.Sender,
		"tekton.dev/pipeline-ascode-branch":     runinfo.BaseBranch,
	}

	// Create the actual pipeline
	pr, err := cs.Tekton.TektonV1beta1().PipelineRuns(repo.Spec.Namespace).Create(ctx, pipelineRun, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// Get the UI/webconsole URL for this pipeline to watch the log (only openshift console suppported atm)
	consoleURL, err := k8int.GetConsoleUI(ctx, repo.Spec.Namespace, pr.GetName())
	if err != nil {
		// Don't bomb out if we can't get the console UI
		consoleURL = "https://giphy.com/explore/cat-exercise-wheel"
	}

	// Create status with the log url
	_, err = cs.GithubClient.CreateStatus(ctx, runinfo, "in_progress", "",
		fmt.Sprintf(`Starting Pipelinerun <b>%s</b> in namespace <b>%s</b><br><br>You can follow the execution on the command line with : <br><br><code>tkn pr logs -f -n %s %s</code>`,
			pr.GetName(), repo.Spec.Namespace, repo.Spec.Namespace, pr.GetName()),
		consoleURL)
	if err != nil {
		return nil
	}

	// Use this as a wait holder until the logs is finished, maybe we would do something with the log output.
	// TODO: to remove and use just a simple wait for deployment
	_, err = k8int.TektonCliFollowLogs(repo.Spec.Namespace, pr.GetName())
	if err != nil {
		return err
	}

	// Post the final status to GitHub check status with a nice breakdown and
	// tekton cli describe output.
	newPr, err := postFinalStatus(ctx, cs, k8int, runinfo, pr.Name, repo.Spec.Namespace)
	if err != nil {
		return err
	}

	repoStatus := apipac.RepositoryRunStatus{
		Status:          newPr.Status.Status,
		PipelineRunName: newPr.Name,
		StartTime:       newPr.Status.StartTime,
		CompletionTime:  newPr.Status.CompletionTime,
	}

	// Get repo again in case it was updated while we were running the CI
	// NOTE: there may be a race issue we should maybe solve here, between the Get and
	// Update but we are talking sub-milliseconds issue here.
	lastrepo, err := cs.PipelineAsCode.PipelinesascodeV1alpha1().Repositories(repo.Spec.Namespace).Get(ctx, repo.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// Append pipelinerun status files to the repo status
	if len(lastrepo.Status) >= maxPipelineRunStatusRun {
		copy(lastrepo.Status, lastrepo.Status[len(lastrepo.Status)-maxPipelineRunStatusRun+1:])
		lastrepo.Status = lastrepo.Status[:maxPipelineRunStatusRun-1]
	}
	lastrepo.Status = append(lastrepo.Status, repoStatus)
	nrepo, err := cs.PipelineAsCode.PipelinesascodeV1alpha1().Repositories(lastrepo.Namespace).Update(
		ctx, lastrepo, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	cs.Log.Infof("Repository status of %s has been updated", nrepo.Name)
	return err
}
