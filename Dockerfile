FROM gcr.io/distroless/static-debian11:nonroot
ENTRYPOINT ["/baton-temporalcloud"]
COPY baton-temporalcloud /