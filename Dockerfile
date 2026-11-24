FROM scratch
COPY bin/ /app/bin/
COPY bin/portmesh-linux-amd64 /app/portmesh
ENTRYPOINT ["/app/portmesh", "controller"]
