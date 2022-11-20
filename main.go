package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

func reqDaemon(method, op string, query url.Values) (*http.Response, error) {
	rawQuery := ""
	if query != nil {
		rawQuery = query.Encode()
	}
	return http.DefaultClient.Do(&http.Request{
		Method: method,
		URL:    &url.URL{Scheme: "http", Host: daemonAddr, Path: "/" + op, RawQuery: rawQuery},
	})
}

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "run the tsdocker daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := newDaemon()
			if err != nil {
				return err
			}
			c := make(chan os.Signal, 1)
			signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-c
				log.Println("shutting down...")
				d.stop()
				os.Exit(1)
			}()
			d.run()
			return nil
		},
	}
}

func newRunCmd() *cobra.Command {
	image, name := "", ""
	port := 0
	cmd := &cobra.Command{
		Use:   "run",
		Short: "run an instance",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				b := make([]byte, 4)
				for i := range b {
					b[i] = byte(rand.Int())
				}
				name = hex.EncodeToString(b)
			}
			res, err := reqDaemon(http.MethodPost, "run", url.Values{
				"image": {image},
				"name":  {name},
				"port":  {strconv.Itoa(port)},
			})
			if err != nil {
				return err
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				data, err := io.ReadAll(res.Body)
				if err != nil {
					return err
				}
				return fmt.Errorf("failed to run instance: %s %s", res.Status, string(data))
			}
			i := Instance{}
			if err = json.NewDecoder(res.Body).Decode(&i); err != nil {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&image, "image", "i", "", "image name")
	_ = cmd.MarkFlagRequired("image")
	cmd.Flags().StringVarP(&name, "name", "n", "", "container name")
	cmd.Flags().IntVarP(&port, "port", "p", 0, "port to expose via Tailscale")
	_ = cmd.MarkFlagRequired("port")
	return cmd
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls", "ps"},
		Short:   "list all running instances",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := reqDaemon(http.MethodGet, "list", nil)
			if err != nil {
				return err
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				data, err := io.ReadAll(res.Body)
				if err != nil {
					return err
				}
				return fmt.Errorf("failed to list instances: %s %s", res.Status, string(data))
			}
			var instances []Instance
			if err = json.NewDecoder(res.Body).Decode(&instances); err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tIMAGE\tTAILSCALE IPS\tPORT")
			for _, i := range instances {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", i.Name, i.Image, strings.Join(i.TailscaleIPs, ","), i.Port)
			}
			return w.Flush()

		},
	}
}
func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "stop an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := reqDaemon(http.MethodDelete, "stop", url.Values{
				"name": {args[0]},
			})
			if err != nil {
				return err
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				data, err := io.ReadAll(res.Body)
				if err != nil {
					return err
				}
				return fmt.Errorf("failed to stop instance: %s %s", res.Status, string(data))
			}
			return nil
		},
	}
}

var daemonAddr string

func main() {
	rootCmd := &cobra.Command{
		Use:           "tsdocker",
		Short:         "tsdocker is a tool to run Tailscale accessible docker containers ",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	rootCmd.PersistentFlags().StringVarP(&daemonAddr, "daemon-address", "d", "0.0.0.0:8080", "daemon address")
	rootCmd.AddCommand(newServeCmd())
	rootCmd.AddCommand(newRunCmd())
	rootCmd.AddCommand(newListCmd())
	rootCmd.AddCommand(newStopCmd())

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}
