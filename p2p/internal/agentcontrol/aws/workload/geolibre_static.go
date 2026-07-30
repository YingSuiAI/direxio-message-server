package coreworkload

import (
	"encoding/base64"
	"fmt"
)

// GeoLibreStaticV1CommandSteps returns a fresh copy of the only command
// program accepted by the geolibre-static-v1 profile. Keeping the audited
// program beside profile validation prevents a caller from relabeling
// arbitrary SSM shell as this release.
func GeoLibreStaticV1CommandSteps() []string {
	nginxTemplate := base64.StdEncoding.EncodeToString([]byte(geoLibreStaticV1NginxTemplate))
	return []string{
		"set -euo pipefail",
		`test "$(uname -m)" = "x86_64"`,
		"dnf install -y docker curl",
		"systemctl enable --now docker",
		"docker pull --platform linux/amd64 " + GeoLibreStaticV1ImageURI,
		"install -d -m 0700 /var/lib/dirextalk-geolibre",
		"printf '%s' '" + nginxTemplate + "' | base64 -d > /var/lib/dirextalk-geolibre/nginx.conf.template",
		"chmod 0600 /var/lib/dirextalk-geolibre/nginx.conf.template",
		`printf '%s\n' '[Unit]' 'Description=Dirextalk GeoLibre' 'After=docker.service network-online.target' 'Requires=docker.service' '' '[Service]' 'Restart=always' 'RestartSec=5' 'ExecStartPre=-/usr/bin/docker rm -f dirextalk-geolibre' 'ExecStart=/usr/bin/docker run --name dirextalk-geolibre --pull=never --platform linux/amd64 --security-opt no-new-privileges --memory 2g --cpus 2 -e GEOLIBRE_DISABLE_SIDECAR=1 -e VITE_WELCOME_DISABLED=1 -p 80:80 -v /var/lib/dirextalk-geolibre/nginx.conf.template:/etc/nginx/nginx.conf.template:ro ` + GeoLibreStaticV1ImageURI + `' 'ExecStop=/usr/bin/docker stop dirextalk-geolibre' '' '[Install]' 'WantedBy=multi-user.target' > /etc/systemd/system/` + GeoLibreStaticV1Service,
		"systemctl daemon-reload",
		"systemctl enable --now " + GeoLibreStaticV1Service,
		"curl --fail --silent --show-error --max-time 10 http://127.0.0.1/healthz",
	}
}

// GeoLibreStaticV1NginxTemplate returns the audited public static-only server
// template. It contains no secret and is exposed for contract verification.
func GeoLibreStaticV1NginxTemplate() string { return geoLibreStaticV1NginxTemplate }

// GeoLibreStaticV1Summary is the exact owner-facing disclosure bound into the
// plan digest and confirmation. Only a canonical provision UUID varies.
func GeoLibreStaticV1Summary(provisionID string) string {
	if !ValidUUID(provisionID) {
		return ""
	}
	return fmt.Sprintf(
		"Install GeoLibre %s on provision %s as public unauthenticated HTTP (no TLS); writable sidecar and persistent /data are disabled; destroy removes the systemd unit, container, pinned image, and generated config but retains the EC2 provision",
		GeoLibreStaticV1Release,
		provisionID,
	)
}

const geoLibreStaticV1NginxTemplate = `server {
    listen 80;
    server_name _;
    root /usr/share/nginx/html;
    index index.html;
    location = /healthz {
        access_log off;
        default_type text/plain;
        return 200 "ok\n";
    }
    location / {
        try_files $uri /index.html;
        add_header Cache-Control "no-cache, must-revalidate" always;
        add_header X-Content-Type-Options "nosniff" always;
        add_header Referrer-Policy "no-referrer" always;
        add_header Content-Security-Policy "default-src 'self'; connect-src 'self' https: data: blob: wss://collab.geolibre.app; img-src 'self' data: blob: https:; media-src 'self' blob: https:; style-src 'self' 'unsafe-inline'; script-src 'self' blob: 'unsafe-eval' 'wasm-unsafe-eval' https://cdn.jsdelivr.net/npm/ https://cdn.jsdelivr.net/pyodide/ https://accounts.google.com; child-src 'self' https://accounts.google.com https://www.google.com; frame-src 'self' https://accounts.google.com https://www.google.com; worker-src blob: 'self'" always;
    }
    location /sidecar/ { return 404; }
}`
