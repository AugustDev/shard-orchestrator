package float

import (
	_ "embed"
	"fmt"
	"log/slog"
	"nf-shard-orchestrator/pkg/runner"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var _ runner.Runner = &Service{}

//go:embed config/job_submit_AWS.sh
var fileJobSubmitAWS string

//go:embed config/transient_JFS_AWS.sh
var fileTransientJFSAWS string

const configOverrideNeedle = "SHARD_CONFIG_OVERRIDE"
const configNextflowCmdNeedle = "SHARD_NEXTFLOW_COMMAND"

type Config struct {
	Logger          *slog.Logger
	Wg              *sync.WaitGroup
	FloatBinPath    string
	NextflowBinPath string
}

type Service struct {
	config Config
	Wg     *sync.WaitGroup
	Logger *slog.Logger
}

func NewRunner(c Config) *Service {
	return &Service{
		config: c,
		Wg:     c.Wg,
		Logger: c.Logger,
	}
}

func (s *Service) auth() error {
	user := os.Getenv("FLOAT_USER")
	pass := os.Getenv("FLOAT_PASS")
	address := os.Getenv("FLOAT_ADDRESS")
	args := []string{"login", "-a", address, "-u", user, "-p", pass}

	return exec.Command(s.config.FloatBinPath, args...).Run()
}

func extractMounts(configOverride string) []string {
	mounts := make([]string, 0)
	pattern := `--dataVolume\s+(\[.*?\][^\s]+)`
	re := regexp.MustCompile(pattern)
	matches := re.FindAllStringSubmatch(configOverride, -1)

	for _, match := range matches {
		if len(match) > 1 {
			// Trim any leading/trailing whitespace
			mounts = append(mounts, strings.TrimSpace(match[1]))
			fmt.Println(strings.TrimSpace(match[1]))
		}
	}
	return mounts
}

func injectConfig(configOverride string, nfCommand string) string {
	nfConfig := fmt.Sprintf(`
export GITHUB_TOKEN=%s
nextflow_command='%s'`, os.Getenv("GITHUB_TOKEN"), nfCommand)

	config := fileJobSubmitAWS

	// injecting config overrides
	config = strings.Replace(config, configOverrideNeedle, configOverride, 1)

	// injecting nextflow command
	config = strings.Replace(config, configNextflowCmdNeedle, nfConfig, 1)

	return config
}

func (s *Service) storeJobFiles(tempDir string, configOverride string, nfCommand string) error {
	files := map[string]string{
		"job_submit_AWS.sh":    injectConfig(configOverride, nfCommand),
		"transient_JFS_AWS.sh": fileTransientJFSAWS,
	}

	for filename, content := range files {
		filePath := filepath.Join(tempDir, filename)
		err := os.WriteFile(filePath, []byte(content), 0644)
		if err != nil {
			s.Logger.Error("Failed to write file", "filename", filename, "error", err)
			return err
		}
	}

	return nil
}

func (s *Service) MockExecute(run runner.RunConfig, injectedConfigPath string) error {
	args := run.CmdArgs()
	args = append(args, "-c", injectedConfigPath)

	command := exec.Command(s.config.NextflowBinPath, args...)
	output, err := command.CombinedOutput()

	if err != nil {
		s.Logger.Debug("nextflow mock output", "output", string(output))
		return err
	}
	s.Logger.Debug("nextflow mock output", "output", string(output))
	return nil
}

func (s *Service) Execute(run runner.RunConfig) string {
	s.Wg.Add(1)
	defer s.Wg.Done()

	// work dir will be manaaaged by float
	run = run.RemoveWorkDir()

	tempDir, err := os.MkdirTemp("", "float-runner-")
	if err != nil {
		s.Logger.Error("Failed to create temporary directory", "error", err)
		return ""
	}
	defer os.RemoveAll(tempDir)

	// generate nextflow command
	nfArgs := append([]string{s.config.NextflowBinPath, "run", run.PipelineUrl, "-c", "mmc.config"}, run.Args...)
	nfCommand := strings.Join(nfArgs, " ")
	fmt.Println(nfCommand)

	err = s.storeJobFiles(tempDir, run.ConfigOverride, nfCommand)
	if err != nil {
		return ""
	}

	mounts := extractMounts(run.ConfigOverride)
	fmt.Println("mounts", mounts)

	err = s.auth()
	if err != nil {
		s.Logger.Error("failed to authenticate", "error", err)
		return ""
	}

	args := []string{
		"submit",
		"--hostInit", filepath.Join(tempDir, "transient_JFS_AWS.sh"),
		"-i", "docker.io/memverge/juiceflow",
		"--vmPolicy", "[onDemand=true]",
		"--migratePolicy", "[disable=true]",
		"--dataVolume", "[size=120]:/mnt/jfs_cache",
		"--dataVolume", "[endpoint=s3.us-east-1.amazonaws.com,mode=r]s3://cfdx-experiments/:/cfdx-experiments",
		"--dataVolume", "[endpoint=s3.us-east-1.amazonaws.com,mode=r]s3://cfdx-research-data/:/cfdx-research-data",
		"--dataVolume", "[endpoint=s3.us-east-1.amazonaws.com,mode=r]s3://cfdx-raw-data/:/cfdx-raw-data",
		"--dirMap", "/mnt/jfs:/mnt/jfs",
		"-c", "8",
		"-m", "16",
		"-n", "shard-run",
		"--securityGroup", "sg-0e3a2750bdf58794c",
		"--env", "BUCKET=https://cfdx-juicefs2.s3.us-east-1.amazonaws.com",
		"-j", filepath.Join(tempDir, "job_submit_AWS.sh"),
	}

	cmd := exec.Command(s.config.FloatBinPath, args...)
	cmd.Dir = tempDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		s.Logger.Debug("float exec error", "error", err, "output", output)
		return ""
	}
	s.Logger.Debug("float exec output", "output", string(output))
	return ""
}

func (s *Service) Stop(c runner.StopConfig) error {
	// not implemented
	return nil
}

func (s *Service) BinPath() string {
	return s.config.FloatBinPath
}
