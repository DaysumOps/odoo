// Copyright (c) 2019 ACSONE SA/NV
// Distributed under the MIT License (http://opensource.org/licenses/MIT)

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

const chunkSize = 32 * 1024

// Common wkhtmltopdf flags that don't require file output
var directOutputFlags = map[string]bool{
	"-h":              true,
	"--help":          true,
	"-H":              true,
	"-v":              true,
	"--version":       true,
	"--extended-help": true,
	"--readme":        true,
	"--manpage":       true,
	"--license":       true,
	"--htmldoc":       true,
	"--list-plugins":  true,
}

func isDirectOutputFlag(args []string) bool {
	for _, arg := range args {
		if directOutputFlags[arg] {
			return true
		}
	}
	return false
}

func addOption(w *multipart.Writer, option string) error {
	return w.WriteField("option", option)
}

func addFile(w *multipart.Writer, filename string) error {
	writer, err := w.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(writer, file)
	return err
}

func executeLocalWkhtmltopdf(args []string, out *os.File) error {
	wkhtmltopdfPath := os.Getenv("WKHTMLTOPDF_PATH")
	if wkhtmltopdfPath == "" {
		return fmt.Errorf("WKHTMLTOPDF_PATH environment variable must be set with the full path to wkhtmltopdf binary")
	}

	// Special handling for flags that output directly to stdout
	if isDirectOutputFlag(args) {
		cmd := exec.Command(wkhtmltopdfPath, args...)
		cmd.Stdout = out
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Get the output file path
	outputPath := out.Name()
	isStdout := outputPath == "/dev/stdout" || outputPath == ""

	if isStdout {
		// If writing to stdout, create a temporary file
		tmpFile, err := os.CreateTemp("", "wkhtmltopdf-*.pdf")
		if err != nil {
			return fmt.Errorf("failed to create temp file: %v", err)
		}
		defer os.Remove(tmpFile.Name())
		outputPath = tmpFile.Name()
	} else {
		// Close the original file as wkhtmltopdf will handle the output
		out.Close()
	}

	// Process arguments to handle various flag formats
	var processedArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		// Handle flags with values (--flag value or --flag=value)
		if strings.HasPrefix(arg, "--") {
			if strings.Contains(arg, "=") {
				// Handle --flag=value format
				processedArgs = append(processedArgs, arg)
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				// Handle --flag value format
				processedArgs = append(processedArgs, arg, args[i+1])
				i++ // Skip the next argument since we've already processed it
			} else {
				// Handle standalone flags
				processedArgs = append(processedArgs, arg)
			}
		} else if strings.HasPrefix(arg, "-") {
			// Handle short flags (-f value or -f)
			if len(arg) == 2 && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				// Handle -f value format
				processedArgs = append(processedArgs, arg, args[i+1])
				i++ // Skip the next argument
			} else {
				// Handle standalone flags or combined short flags (-abc)
				processedArgs = append(processedArgs, arg)
			}
		} else {
			// Handle non-flag arguments (input files, etc.)
			processedArgs = append(processedArgs, arg)
		}
	}

	// Add output path as the last argument
	processedArgs = append(processedArgs, outputPath)

	// Execute wkhtmltopdf with processed arguments
	cmd := exec.Command(wkhtmltopdfPath, processedArgs...)
	cmd.Stderr = os.Stderr

	// Execute the command
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wkhtmltopdf execution failed: %v", err)
	}

	// If original output was stdout, copy the content
	if isStdout {
		pdfFile, err := os.Open(outputPath)
		if err != nil {
			return fmt.Errorf("failed to read output file: %v", err)
		}
		defer pdfFile.Close()

		_, err = io.Copy(out, pdfFile)
		if err != nil {
			return fmt.Errorf("failed to write to stdout: %v", err)
		}
	}

	return nil
}

func do() error {
	var err error
	var out *os.File

	serverURL := os.Getenv("KWKHTMLTOPDF_SERVER_URL")
	useLocal := serverURL == ""

	// detect if last argument is output file, and create it
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"-h"}
	}
	if len(args) >= 2 && !strings.HasPrefix(args[len(args)-1], "-") && !strings.HasPrefix(args[len(args)-2], "-") {
		out, err = os.Create(args[len(args)-1])
		if err != nil {
			return err
		}
		defer out.Close()
		args = args[:len(args)-1]
	} else {
		out = os.Stdout
	}

	if useLocal {
		fmt.Fprintf(os.Stderr, "KWKHTMLTOPDF_SERVER_URL not set, falling back to local wkhtmltopdf...\n")
		return executeLocalWkhtmltopdf(args, out)
	}

	// prepare request
	var postBuf bytes.Buffer
	w := multipart.NewWriter(&postBuf)
	for _, arg := range args {
		if arg == "-" {
			return errors.New("stdin/stdout input is not implemented")
		} else if strings.HasPrefix(arg, "-") {
			err = addOption(w, arg)
		} else if strings.HasPrefix(arg, "https://") {
			err = addOption(w, arg)
		} else if strings.HasPrefix(arg, "http://") {
			err = addOption(w, arg)
		} else if strings.HasPrefix(arg, "file://") {
			err = addFile(w, arg[7:])
		} else if _, err := os.Stat(arg); err == nil {
			// TODO: better way to detect file arguments
			err = addFile(w, arg)
		} else {
			err = addOption(w, arg)
		}
		if err != nil {
			return err
		}
	}
	w.Close()

	// post request
	resp, err := http.Post(serverURL, w.FormDataContentType(), &postBuf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to server: %v\nFalling back to local wkhtmltopdf...\n", err)
		return executeLocalWkhtmltopdf(args, out)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Server returned error status: %d\nFalling back to local wkhtmltopdf...\n", resp.StatusCode)
		return executeLocalWkhtmltopdf(args, out)
	}

	// read response
	respBuf := make([]byte, chunkSize)
	for {
		nr, er := resp.Body.Read(respBuf)
		if er != nil && er != io.EOF {
			return errors.New("server error, consult server log for details")
		}
		if nr > 0 {
			_, ew := out.Write(respBuf[0:nr])
			if ew != nil {
				return ew
			}
		}
		if er == io.EOF {
			break
		}
	}

	return nil
}

func main() {
	err := do()
	if err != nil {
		os.Stderr.WriteString(err.Error())
		os.Stderr.WriteString("\n")
		os.Exit(-1)
	}
}
