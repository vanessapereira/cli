package application

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"sync"

	"github.com/cloudfoundry/cli/cf"
	"github.com/cloudfoundry/cli/cf/api/appinstances"
	"github.com/cloudfoundry/cli/cf/api/applications"
	"github.com/cloudfoundry/cli/cf/api/logs"
	"github.com/cloudfoundry/cli/cf/commandregistry"
	"github.com/cloudfoundry/cli/cf/configuration/coreconfig"
	. "github.com/cloudfoundry/cli/cf/i18n"
	"github.com/cloudfoundry/cli/cf/models"
	"github.com/cloudfoundry/cli/cf/requirements"
	"github.com/cloudfoundry/cli/cf/terminal"
	"github.com/cloudfoundry/cli/flags"
)

const (
	DefaultStagingTimeout = 15 * time.Minute
	DefaultStartupTimeout = 5 * time.Minute
	DefaultPingerThrottle = 5 * time.Second
)

const LogMessageTypeStaging = "STG"

//go:generate counterfeiter . StagingWatcher

type StagingWatcher interface {
	WatchStaging(app models.Application, orgName string, spaceName string, startCommand func(app models.Application) (models.Application, error)) (updatedApp models.Application, err error)
}

//go:generate counterfeiter . Starter

type Starter interface {
	commandregistry.Command
	SetStartTimeoutInSeconds(timeout int)
	ApplicationStart(app models.Application, orgName string, spaceName string) (updatedApp models.Application, err error)
}

type Start struct {
	ui               terminal.UI
	config           coreconfig.Reader
	appDisplayer     Displayer
	appReq           requirements.ApplicationRequirement
	appRepo          applications.Repository
	logRepo          logs.Repository
	appInstancesRepo appinstances.Repository

	LogServerConnectionTimeout time.Duration
	StartupTimeout             time.Duration
	StagingTimeout             time.Duration
	PingerThrottle             time.Duration
}

func init() {
	commandregistry.Register(&Start{})
}

func (cmd *Start) MetaData() commandregistry.CommandMetadata {
	return commandregistry.CommandMetadata{
		Name:        "start",
		ShortName:   "st",
		Description: T("Start an app"),
		Usage: []string{
			T("CF_NAME start APP_NAME"),
		},
	}
}

func (cmd *Start) Requirements(requirementsFactory requirements.Factory, fc flags.FlagContext) []requirements.Requirement {
	if len(fc.Args()) != 1 {
		cmd.ui.Failed(T("Incorrect Usage. Requires an argument\n\n") + commandregistry.Commands.CommandUsage("start"))
	}

	cmd.appReq = requirementsFactory.NewApplicationRequirement(fc.Args()[0])

	reqs := []requirements.Requirement{
		requirementsFactory.NewLoginRequirement(),
		requirementsFactory.NewTargetedSpaceRequirement(),
		cmd.appReq,
	}

	return reqs
}

func (cmd *Start) SetDependency(deps commandregistry.Dependency, pluginCall bool) commandregistry.Command {
	cmd.ui = deps.UI
	cmd.config = deps.Config
	cmd.appRepo = deps.RepoLocator.GetApplicationRepository()
	cmd.appInstancesRepo = deps.RepoLocator.GetAppInstancesRepository()
	cmd.logRepo = deps.RepoLocator.GetLogsRepository()
	cmd.LogServerConnectionTimeout = 20 * time.Second
	cmd.PingerThrottle = DefaultPingerThrottle

	if os.Getenv("CF_STAGING_TIMEOUT") != "" {
		duration, err := strconv.ParseInt(os.Getenv("CF_STAGING_TIMEOUT"), 10, 64)
		if err != nil {
			cmd.ui.Failed(T("invalid value for env var CF_STAGING_TIMEOUT\n{{.Err}}",
				map[string]interface{}{"Err": err}))
		}
		cmd.StagingTimeout = time.Duration(duration) * time.Minute
	} else {
		cmd.StagingTimeout = DefaultStagingTimeout
	}

	if os.Getenv("CF_STARTUP_TIMEOUT") != "" {
		duration, err := strconv.ParseInt(os.Getenv("CF_STARTUP_TIMEOUT"), 10, 64)
		if err != nil {
			cmd.ui.Failed(T("invalid value for env var CF_STARTUP_TIMEOUT\n{{.Err}}",
				map[string]interface{}{"Err": err}))
		}
		cmd.StartupTimeout = time.Duration(duration) * time.Minute
	} else {
		cmd.StartupTimeout = DefaultStartupTimeout
	}

	appCommand := commandregistry.Commands.FindCommand("app")
	appCommand = appCommand.SetDependency(deps, false)
	cmd.appDisplayer = appCommand.(Displayer)

	return cmd
}

func (cmd *Start) Execute(c flags.FlagContext) error {
	_, err := cmd.ApplicationStart(cmd.appReq.GetApplication(), cmd.config.OrganizationFields().Name, cmd.config.SpaceFields().Name)
	return err
}

func (cmd *Start) ApplicationStart(app models.Application, orgName, spaceName string) (models.Application, error) {
	if app.State == "started" {
		cmd.ui.Say(terminal.WarningColor(T("App ") + app.Name + T(" is already started")))
		return models.Application{}, nil
	}

	return cmd.WatchStaging(app, orgName, spaceName, func(app models.Application) (models.Application, error) {
		fmt.Println("begin start func")
		cmd.ui.Say(T("Starting app {{.AppName}} in org {{.OrgName}} / space {{.SpaceName}} as {{.CurrentUser}}...",
			map[string]interface{}{
				"AppName":     terminal.EntityNameColor(app.Name),
				"OrgName":     terminal.EntityNameColor(orgName),
				"SpaceName":   terminal.EntityNameColor(spaceName),
				"CurrentUser": terminal.EntityNameColor(cmd.config.Username())}))

		state := "STARTED"
		fmt.Printf("end start func")
		return cmd.appRepo.Update(app.GUID, models.AppParams{State: &state})
	})
}

func (cmd *Start) WatchStaging(app models.Application, orgName, spaceName string, start func(app models.Application) (models.Application, error)) (models.Application, error) {
	fmt.Println("begin WatchStaging")
	stopChan := make(chan bool, 1)

	loggingStartedWait := new(sync.WaitGroup)
	loggingStartedWait.Add(1)

	loggingDoneWait := new(sync.WaitGroup)
	loggingDoneWait.Add(1)

	fmt.Println("kicking off TailStagingLogs")
	go cmd.TailStagingLogs(app, stopChan, loggingStartedWait, loggingDoneWait)
	fmt.Println("kicked off TailStagingLogs")
	fmt.Println("loggingStartedWait waiting")
	loggingStartedWait.Wait()
	fmt.Println("loggingStartedWait released")

	updatedApp, err := start(app)
	if err != nil {
		fmt.Printf("start func returned err: %s\n", err.Error())
		return models.Application{}, err
	}

	isStaged, err := cmd.waitForInstancesToStage(updatedApp)
	if err != nil {
		fmt.Printf("waitForInstancesToStage returned err: %s\n", err.Error())
		return models.Application{}, err
	}
	fmt.Println("write to stopChan")
	stopChan <- true
	fmt.Println("loggingDoneWait waiting")
	loggingDoneWait.Wait()
	fmt.Println("loggingDoneWait released")

	cmd.ui.Say("")

	if !isStaged {
		return models.Application{}, fmt.Errorf("%s failed to stage within %f minutes", app.Name, cmd.StagingTimeout.Minutes())
	}

	err = cmd.waitForOneRunningInstance(updatedApp)
	if err != nil {
		fmt.Printf("waitForOneRunningInstance returned err: %s\n", err.Error())
		return models.Application{}, err
	}
	cmd.ui.Say(terminal.HeaderColor(T("\nApp started\n")))
	cmd.ui.Say("")
	cmd.ui.Ok()

	//detectedstartcommand on first push is not present until starting completes
	startedApp, err := cmd.appRepo.GetApp(updatedApp.GUID)
	if err != nil {
		return models.Application{}, err
	}

	var appStartCommand string
	if app.Command == "" {
		appStartCommand = startedApp.DetectedStartCommand
	} else {
		appStartCommand = startedApp.Command
	}

	cmd.ui.Say(T("\nApp {{.AppName}} was started using this command `{{.Command}}`\n",
		map[string]interface{}{
			"AppName": terminal.EntityNameColor(startedApp.Name),
			"Command": appStartCommand,
		}))

	err = cmd.appDisplayer.ShowApp(startedApp, orgName, spaceName)
	if err != nil {
		return models.Application{}, err
	}

	return updatedApp, nil
}

func (cmd *Start) SetStartTimeoutInSeconds(timeout int) {
	cmd.StartupTimeout = time.Duration(timeout) * time.Second
}

type ConnectionType int

const (
	NoConnection ConnectionType = iota
	ConnectionWasEstablished
	ConnectionWasClosed
	StoppedTrying
)

func (cmd *Start) TailStagingLogs(app models.Application, stopChan chan bool, startWait, doneWait *sync.WaitGroup) {
	fmt.Println("begin TailStagingLogs")
	var connectionStatus ConnectionType
	connectionStatus = NoConnection

	onConnect := func() {
		fmt.Println("begin onConnect")
		if connectionStatus != StoppedTrying {
			fmt.Println("onConnect connectionStatus!=StoppedTrying")
			connectionStatus = ConnectionWasEstablished
			startWait.Done()
		}
		fmt.Println("end onConnect")
	}

	timer := time.NewTimer(cmd.LogServerConnectionTimeout)

	c := make(chan logs.Loggable)
	e := make(chan error)

	defer doneWait.Done()
	fmt.Println("kicking off TailLogsFor")
	go cmd.logRepo.TailLogsFor(app.GUID, onConnect, c, e)
	fmt.Println("kicked off TailLogsFor")
	for {
		select {
		case <-timer.C:
			fmt.Println("local timer triggered")
			if connectionStatus == NoConnection {
				fmt.Println("local timer triggered connectionStatus==NoConnection")
				connectionStatus = StoppedTrying
				cmd.ui.Warn("timeout connecting to log server, no log will be shown")
				startWait.Done()
				fmt.Println("local timer triggered return")
				return
			}
			fmt.Println("local timer triggered continue")
		case msg, ok := <-c:
			fmt.Println("message received on channel")
			if !ok {
				fmt.Println("message received on channel broken")
				return
			} else if msg.GetSourceName() == LogMessageTypeStaging {
				fmt.Println("message received on channel logging")
				cmd.ui.Say(msg.ToSimpleLog())
			}

		case err, ok := <-e:
			fmt.Println("error received on channel")
			if ok {
				if connectionStatus != ConnectionWasClosed {
					fmt.Println("error received on channel connectionStatus!=ConnectionWasClosed")
					cmd.ui.Warn(T("Warning: error tailing logs"))
					cmd.ui.Say("%s", err)
					if connectionStatus == NoConnection {
						fmt.Println("error received on channel connectionStatus==NoConnection")
						startWait.Done()
					}
					fmt.Println("error received on channel return")
					return
				}
			}
			fmt.Println("error received on channel continue")

		case <-stopChan:
			fmt.Println("stopChan received")
			if connectionStatus == ConnectionWasEstablished {
				fmt.Println("stopChan received connectionStatus==ConnectionWasEstablished")
				connectionStatus = ConnectionWasClosed
				cmd.logRepo.Close()
			} else {
				fmt.Println("stopChan received return")
				return
			}
		}
	}
}

func (cmd *Start) waitForInstancesToStage(app models.Application) (bool, error) {
	fmt.Println("begin waitForInstancesToStage")
	stagingStartTime := time.Now()

	var err error

	if cmd.StagingTimeout == 0 {
		fmt.Println("waitForInstancesToStage try once")
		app, err = cmd.appRepo.GetApp(app.GUID)
	} else {
		fmt.Println("waitForInstancesToStage try repeat")
		for app.PackageState != "STAGED" && app.PackageState != "FAILED" && time.Since(stagingStartTime) < cmd.StagingTimeout {
			fmt.Println("waitForInstancesToStage trying")
			app, err = cmd.appRepo.GetApp(app.GUID)
			if err != nil {
				break
			}
			fmt.Println("waitForInstancesToStage sleep")
			time.Sleep(cmd.PingerThrottle)
		}
	}

	if err != nil {
		fmt.Println("waitForInstancesToStage return err")
		return false, err
	}

	if app.PackageState == "FAILED" {
		fmt.Println("waitForInstancesToStage PackageState==FAILED")
		cmd.ui.Say("")
		if app.StagingFailedReason == "NoAppDetectedError" {
			return false, errors.New(T(`{{.Err}}
			
TIP: Buildpacks are detected when the "{{.PushCommand}}" is executed from within the directory that contains the app source code.

Use '{{.BuildpackCommand}}' to see a list of supported buildpacks.

Use '{{.Command}}' for more in depth log information.`,
				map[string]interface{}{
					"Err":              app.StagingFailedReason,
					"PushCommand":      terminal.CommandColor(fmt.Sprintf("%s push", cf.Name)),
					"BuildpackCommand": terminal.CommandColor(fmt.Sprintf("%s buildpacks", cf.Name)),
					"Command":          terminal.CommandColor(fmt.Sprintf("%s logs %s --recent", cf.Name, app.Name))}))
		}
		return false, errors.New(T("{{.Err}}\n\nTIP: use '{{.Command}}' for more information",
			map[string]interface{}{
				"Err":     app.StagingFailedReason,
				"Command": terminal.CommandColor(fmt.Sprintf("%s logs %s --recent", cf.Name, app.Name))}))
	}

	if time.Since(stagingStartTime) >= cmd.StagingTimeout {
		fmt.Println("waitForInstancesToStage timeout")
		return false, nil
	}

	fmt.Println("end waitForInstancesToStage")
	return true, nil
}

func (cmd *Start) waitForOneRunningInstance(app models.Application) error {
	fmt.Println("begin waitForOneRunningInstance")
	timer := time.NewTimer(cmd.StartupTimeout)

	for {
		fmt.Println("waitForOneRunningInstance trying")
		select {
		case <-timer.C:
			fmt.Println("waitForOneRunningInstance timeout")
			tipMsg := T("Start app timeout\n\nTIP: Application must be listening on the right port. Instead of hard coding the port, use the $PORT environment variable.") + "\n\n"
			tipMsg += T("Use '{{.Command}}' for more information", map[string]interface{}{"Command": terminal.CommandColor(fmt.Sprintf("%s logs %s --recent", cf.Name, app.Name))})

			return errors.New(tipMsg)

		default:
			fmt.Println("waitForOneRunningInstance trying")
			count, err := cmd.fetchInstanceCount(app.GUID)
			if err != nil {
				cmd.ui.Warn("Could not fetch instance count: %s", err.Error())
				time.Sleep(cmd.PingerThrottle)
				continue
			}

			cmd.ui.Say(instancesDetails(count))

			if count.running > 0 {
				fmt.Println("waitForOneRunningInstance running instances")
				return nil
			}

			if count.flapping > 0 || count.crashed > 0 {
				fmt.Println("waitForOneRunningInstance crashed instances")
				return fmt.Errorf(T("Start unsuccessful\n\nTIP: use '{{.Command}}' for more information",
					map[string]interface{}{"Command": terminal.CommandColor(fmt.Sprintf("%s logs %s --recent", cf.Name, app.Name))}))
			}
			fmt.Println("waitForOneRunningInstance sleep")
			time.Sleep(cmd.PingerThrottle)
		}
	}
}

type instanceCount struct {
	running         int
	starting        int
	startingDetails map[string]struct{}
	flapping        int
	down            int
	crashed         int
	total           int
}

func (cmd Start) fetchInstanceCount(appGUID string) (instanceCount, error) {
	count := instanceCount{
		startingDetails: make(map[string]struct{}),
	}

	instances, apiErr := cmd.appInstancesRepo.GetInstances(appGUID)
	if apiErr != nil {
		return instanceCount{}, apiErr
	}

	count.total = len(instances)

	for _, inst := range instances {
		switch inst.State {
		case models.InstanceRunning:
			count.running++
		case models.InstanceStarting:
			count.starting++
			if inst.Details != "" {
				count.startingDetails[inst.Details] = struct{}{}
			}
		case models.InstanceFlapping:
			count.flapping++
		case models.InstanceDown:
			count.down++
		case models.InstanceCrashed:
			count.crashed++
		}
	}

	return count, nil
}

func instancesDetails(count instanceCount) string {
	details := []string{fmt.Sprintf(T("{{.RunningCount}} of {{.TotalCount}} instances running",
		map[string]interface{}{"RunningCount": count.running, "TotalCount": count.total}))}

	if count.starting > 0 {
		if len(count.startingDetails) == 0 {
			details = append(details, fmt.Sprintf(T("{{.StartingCount}} starting",
				map[string]interface{}{"StartingCount": count.starting})))
		} else {
			info := []string{}
			for d := range count.startingDetails {
				info = append(info, d)
			}
			sort.Strings(info)
			details = append(details, fmt.Sprintf(T("{{.StartingCount}} starting ({{.Details}})",
				map[string]interface{}{
					"StartingCount": count.starting,
					"Details":       strings.Join(info, ", "),
				})))
		}
	}

	if count.down > 0 {
		details = append(details, fmt.Sprintf(T("{{.DownCount}} down",
			map[string]interface{}{"DownCount": count.down})))
	}

	if count.flapping > 0 {
		details = append(details, fmt.Sprintf(T("{{.FlappingCount}} failing",
			map[string]interface{}{"FlappingCount": count.flapping})))
	}

	if count.crashed > 0 {
		details = append(details, fmt.Sprintf(T("{{.CrashedCount}} crashed",
			map[string]interface{}{"CrashedCount": count.crashed})))
	}

	return strings.Join(details, ", ")
}
