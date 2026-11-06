FROM scratch AS source
COPY A/ /A/
COPY B/ /B/

FROM scratch AS final
COPY --from=source /A/ /app/A/
COPY --from=source /B/ /app/B/
