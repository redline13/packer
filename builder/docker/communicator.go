package docker

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/ActiveState/tail"
	"github.com/mitchellh/packer/packer"
)

type Communicator struct {
	ContainerId  string
	HostDir      string
	ContainerDir string

	lock sync.Mutex
}

func (c *Communicator) Start(remote *packer.RemoteCmd) error {
	// Create a temporary file to store the output. Because of a bug in
	// Docker, sometimes all the output doesn't properly show up. This
	// file will capture ALL of the output, and we'll read that.
	//
	// https://github.com/dotcloud/docker/issues/2625
	outputFile, err := ioutil.TempFile(c.HostDir, "cmd")
	if err != nil {
		return err
	}
	outputFile.Close()

	// This file will store the exit code of the command once it is complete.
	exitFile, err := os.Create( outputFile.Name() + "-exit" ) 
	if err != nil {
		return err
	}
	exitFile.Close()

	cmd := exec.Command("docker", "exec", "-i", c.ContainerId, "/bin/sh" )
	stdin_w, err := cmd.StdinPipe()
	if err != nil {
		// We have to do some cleanup since run was never called
		os.Remove(outputFile.Name())
		os.Remove(exitFile.Name())

		return err
	}

	// Run the actual command in a goroutine so that Start doesn't block
	go c.run(cmd, remote, stdin_w, outputFile, exitFile)

	return nil
}

func (c *Communicator) Upload(dst string, src io.Reader, fi *os.FileInfo) error {
	// Create a temporary file to store the upload
	tempfile, err := ioutil.TempFile(c.HostDir, "upload")
	if err != nil {
		return err
	}
	defer os.Remove(tempfile.Name())

	// Copy the contents to the temporary file
	_, err = io.Copy(tempfile, src)
	tempfile.Close()
	if err != nil {
		return err
	}

	// Copy the file into place by copying the temporary file we put
	// into the shared folder into the proper location in the container
	// Need to remove existing if multiple calls to 'shell' are made.
	cmd := &packer.RemoteCmd{
		Command: fmt.Sprintf("rm -f %s; cp -f %s/%s %s", dst, c.ContainerDir,
			filepath.Base(tempfile.Name()), dst),
	}

	if err := c.Start(cmd); err != nil {
		return err
	}

	// Wait for the copy to complete
	cmd.Wait()
	if cmd.ExitStatus != 0 {
		return fmt.Errorf("Upload failed with non-zero exit status: %d", cmd.ExitStatus)
	}

	return nil
}

func (c *Communicator) UploadDir(dst string, src string, exclude []string) error {
	// Create the temporary directory that will store the contents of "src"
	// for copying into the container.
	td, err := ioutil.TempDir(c.HostDir, "dirupload")
	if err != nil {
		return err
	}
	defer os.RemoveAll(td)

	walkFn := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relpath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		hostpath := filepath.Join(td, relpath)

		// If it is a directory, just create it
		if info.IsDir() {
			return os.MkdirAll(hostpath, info.Mode())
		}

		if info.Mode() & os.ModeSymlink == os.ModeSymlink {
			dest, err := os.Readlink(path)

			if err != nil {
				return err
			}

			return os.Symlink(dest, hostpath)
		}

		// It is a file, copy it over, including mode.
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()

		dst, err := os.Create(hostpath)
		if err != nil {
			return err
		}
		defer dst.Close()

		if _, err := io.Copy(dst, src); err != nil {
			return err
		}

		si, err := src.Stat()
		if err != nil {
			return err
		}

		return dst.Chmod(si.Mode())
	}

	// Copy the entire directory tree to the temporary directory
	if err := filepath.Walk(src, walkFn); err != nil {
		return err
	}

	// Determine the destination directory
	containerSrc := filepath.Join(c.ContainerDir, filepath.Base(td))
	containerDst := dst
	if src[len(src)-1] != '/' {
		containerDst = filepath.Join(dst, filepath.Base(src))
	}

	// Make the directory, then copy into it
	cmd := &packer.RemoteCmd{
		Command: fmt.Sprintf("set -e; mkdir -p %s; command cp -R %s/* %s",
			containerDst, containerSrc, containerDst),
	}
	if err := c.Start(cmd); err != nil {
		return err
	}

	// Wait for the copy to complete
	cmd.Wait()
	if cmd.ExitStatus != 0 {
		return fmt.Errorf("Upload failed with non-zero exit status: %d", cmd.ExitStatus)
	}

	return nil
}

func (c *Communicator) Download(src string, dst io.Writer) error {
	panic("not implemented")
}

// Runs the given command and blocks until completion
func (c *Communicator) run(cmd *exec.Cmd, remote *packer.RemoteCmd, stdin_w io.WriteCloser, outputFile *os.File, exitFile *os.File) {
	// For Docker, remote communication must be serialized since it
	// only supports single execution.
	c.lock.Lock()
	defer c.lock.Unlock()

	// Clean up after ourselves by removing our temporary files
	defer os.Remove(outputFile.Name())
	defer os.Remove(exitFile.Name())

	// Tail the output file and send the data to the stdout listener
	log.Printf( "Starting tail on output file: %s", outputFile.Name() )
	outputTail, err := tail.TailFile(outputFile.Name(), tail.Config{
		Poll:   true,
		ReOpen: true,
		Follow: true,
	})
	if err != nil {
		log.Printf("Error tailing output file: %s", err)
		remote.SetExited(254)
		return
	}
	defer outputTail.Stop()

	// Tail the output file and send the data to the stdout listener
	log.Printf( "Startin tail on exit file: %s", exitFile.Name() )
	exitTail, err := tail.TailFile( exitFile.Name(), tail.Config{
		Poll:   true,
		ReOpen: true,
		Follow: true,
	})
	if err != nil {
		log.Printf("Error tailing exit file: %s", err)
		remote.SetExited(254)
		return
	}
	defer exitTail.Stop()

	// Modify the remote command so that all the output of the commands
	// go to a single file and so that the exit code is redirected to
	// a single file. This lets us determine both when the command
	// is truly complete (because the file will have data), what the
	// exit status is (because Docker loses it because of the pty, not
	// Docker's fault), and get the output (Docker bug).
	remoteCmd := fmt.Sprintf("(%s) >%s 2>&1; echo $? >%s",
		remote.Command,
		filepath.Join(c.ContainerDir, filepath.Base(outputFile.Name())),
		filepath.Join(c.ContainerDir, filepath.Base(exitFile.Name())))

	// Start the command
	log.Printf("Executing in container %s: %#v", c.ContainerId, remoteCmd)
	if err := cmd.Start(); err != nil {
		log.Printf("Error executing: %s", err)
		remote.SetExited(254)
		return
	}

	go func() {
		defer stdin_w.Close()

		// This sleep needs to be here because of the issue linked to below.
		// Basically, without it, Docker will hang on reading stdin forever,
		// and won't see what we write, for some reason.
		//
		// https://github.com/dotcloud/docker/issues/2628
		time.Sleep(2 * time.Second)

		stdin_w.Write([]byte(remoteCmd + "\n"))
	}()

	// Start a goroutine to read all the lines out of the logs. These channels
	// allow us to stop the go-routine and wait for it to be stopped.
	stopTailCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)

		for {
			select {
			case <-outputTail.Dead():
				return
			case line := <-outputTail.Lines:
				if remote.Stdout != nil {
					remote.Stdout.Write([]byte(line.Text + "\n"))
				} else {
					log.Printf("Command stdout: %#v", line.Text)
				}
			case <-time.After(2 * time.Second):
				// If we're done, then return. Otherwise, keep grabbing
				// data. This gives us a chance to flush all the lines
				// out of the tailed file.
				select {
				case <-stopTailCh:
					return
				default:
				}
			}
		}
	}()
	
	log.Printf("Start Listening for exit status output")
	exitStatus := 254
	for { 
		select { 

			case <-exitTail.Dead():
				goto REMOTE_EXIT
			
			case exitRaw := <-exitTail.Lines:
				log.Printf("Exit -exit Output: %#v", exitRaw.Text)
				exitStatusRaw, err := strconv.ParseInt(string(bytes.TrimSpace([]byte(exitRaw.Text))), 10, 0)
				if err != nil {
					log.Printf("Error executing: %s", err)
					goto REMOTE_EXIT
				}
				exitStatus = int(exitStatusRaw)
				log.Printf("Executed command exit status: %d", exitStatus)
				goto REMOTE_EXIT
							
			case <- time.After( 60 * time.Minute ) : 
				log.Printf("Timer Expired, Build failed" )
				goto REMOTE_EXIT
				
		}
	}

REMOTE_EXIT:
	// Wait for the tail to finish
	close(stopTailCh)
	<-doneCh

	// Set the exit status which triggers waiters
	remote.SetExited(exitStatus)
}
