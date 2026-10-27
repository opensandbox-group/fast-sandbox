package netprobe

import "strconv"

const httpGetScript = `exec 3<>"/dev/tcp/$1/$2" || exit 1
printf 'GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n' "$1" >&3 || exit 1
cat <&3 || true`

// HTTPGetCommand returns a dependency-free HTTP probe suitable for the Fastlet
// container. The timeout bounds both connection establishment and response
// reading, while positional arguments keep host and port out of the shell
// program itself.
func HTTPGetCommand(host string, port int) []string {
	return []string{
		"timeout", "2", "bash", "-c", httpGetScript, "--", host, strconv.Itoa(port),
	}
}

// HTTPGetInNetNSCommand runs HTTPGetCommand from the named network namespace.
func HTTPGetInNetNSCommand(netns, host string, port int) []string {
	command := []string{"ip", "netns", "exec", netns}
	return append(command, HTTPGetCommand(host, port)...)
}
