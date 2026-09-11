defmodule Arc.CLI.ProviderBundle do
  @moduledoc """
  Local ARC provider bundle support.

  A provider bundle is a local directory containing:

    * `Arcfile` - local runtime recipe
    * `manifest.json` - served installable capability definition

  `arc serve` resolves a bundle into the low-level `exec://...?...` URI that
  the runtime layer already understands.
  """

  @arcfile_name "Arcfile"
  @manifest_name "manifest.json"
  @runtime_name "run.sh"

  @type bundle_spec :: %{
          root: String.t(),
          arcfile: String.t(),
          runtime: %{
            type: String.t(),
            command: String.t(),
            args: [String.t()],
            cwd: String.t()
          },
          manifest: %{
            path: String.t()
          }
        }

  @spec init(String.t()) :: {:ok, map()} | {:error, term()}
  def init(path) when is_binary(path) and path != "" do
    root = path |> expand_path() |> Path.expand()
    name = bundle_name(root)
    namespace = normalize_namespace(name)

    files = %{
      arcfile: Path.join(root, @arcfile_name),
      manifest: Path.join(root, @manifest_name),
      runtime: Path.join(root, @runtime_name)
    }

    with :ok <- File.mkdir_p(root),
         :ok <- ensure_absent(Map.values(files)) do
      File.write!(files.arcfile, arcfile_template())
      File.write!(files.manifest, manifest_template(namespace, titleize(name)))
      File.write!(files.runtime, runtime_template(namespace))
      File.chmod!(files.runtime, 0o755)

      {:ok, Map.put(files, :root, root)}
    end
  end

  def init(_path), do: {:error, :invalid_path}

  @spec resolve_serve_target(String.t()) ::
          {:ok, String.t(), bundle_spec() | nil} | {:error, term()}
  def resolve_serve_target(target) when is_binary(target) and target != "" do
    cond do
      uri_target?(target) ->
        {:ok, target, nil}

      File.dir?(target) ->
        with {:ok, bundle} <- load_bundle(Path.join(target, @arcfile_name)) do
          {:ok, serve_uri(bundle), bundle}
        end

      File.regular?(target) and Path.basename(target) == @arcfile_name ->
        with {:ok, bundle} <- load_bundle(target) do
          {:ok, serve_uri(bundle), bundle}
        end

      true ->
        {:ok, target, nil}
    end
  end

  def resolve_serve_target(_target), do: {:error, :invalid_target}

  @spec serve_uri(bundle_spec()) :: String.t()
  def serve_uri(bundle) when is_map(bundle) do
    runtime = bundle.runtime

    query =
      %{"manifest" => bundle.manifest.path}
      |> maybe_put_args(runtime.args)
      |> URI.encode_query()

    "exec://#{runtime.command}?#{query}"
  end

  @spec load_bundle(String.t()) :: {:ok, bundle_spec()} | {:error, term()}
  def load_bundle(path) when is_binary(path) and path != "" do
    arcfile = expand_path(path)

    with true <- File.regular?(arcfile) or {:error, :arcfile_not_found},
         {:ok, body} <- File.read(arcfile),
         {:ok, document} <- TomlElixir.decode(body) do
      normalize_bundle(document, Path.dirname(arcfile), arcfile)
    end
  end

  def load_bundle(_path), do: {:error, :invalid_target}

  defp normalize_bundle(document, root, arcfile) when is_map(document) do
    with 1 <- document["version"] || {:error, :unsupported_arcfile_version},
         %{} = runtime <- document["runtime"] || {:error, :missing_runtime},
         "exec" <- present_string(runtime["type"]) || {:error, :unsupported_runtime},
         command when not is_nil(command) <-
           present_string(runtime["command"]) || {:error, :missing_command},
         cwd <- runtime_cwd(root, runtime["cwd"]),
         command_path <- resolve_runtime_command(command, cwd),
         args <- normalize_args(runtime["args"]),
         %{} = manifest <- document["manifest"] || {:error, :missing_manifest},
         manifest_path when not is_nil(manifest_path) <-
           present_string(manifest["path"]) || {:error, :missing_manifest_path},
         manifest_abs <- resolve_path(root, manifest_path),
         true <- File.regular?(manifest_abs) or {:error, :manifest_not_found} do
      {:ok,
       %{
         root: root,
         arcfile: arcfile,
         runtime: %{
           type: "exec",
           command: command_path,
           args: args,
           cwd: cwd
         },
         manifest: %{
           path: manifest_abs
         }
       }}
    else
      {:error, _reason} = error -> error
      _ -> {:error, :invalid_arcfile}
    end
  end

  defp runtime_cwd(root, nil), do: root
  defp runtime_cwd(root, ""), do: root
  defp runtime_cwd(root, cwd), do: resolve_path(root, cwd)

  defp resolve_runtime_command(command, cwd) do
    if Path.type(command) == :absolute do
      command
    else
      resolve_path(cwd, command)
    end
  end

  defp resolve_path(root, path) do
    if Path.type(path) == :absolute do
      Path.expand(path)
    else
      Path.expand(path, root)
    end
  end

  defp normalize_args(nil), do: []
  defp normalize_args([]), do: []

  defp normalize_args(args) when is_list(args) do
    args
    |> Enum.map(&present_string/1)
    |> Enum.reject(&is_nil/1)
  end

  defp normalize_args(_args), do: []

  defp maybe_put_args(query, []), do: query

  defp maybe_put_args(query, args) when is_list(args) do
    Map.put(query, "args", IO.iodata_to_binary(:json.encode(args)))
  end

  defp uri_target?(target) do
    String.match?(target, ~r/^[a-z][a-z0-9+.-]*:\/\//i)
  end

  defp expand_path(path), do: Path.expand(path)

  defp ensure_absent(paths) do
    case Enum.find(paths, &File.exists?/1) do
      nil -> :ok
      path -> {:error, {:already_exists, path}}
    end
  end

  defp bundle_name(root) do
    root
    |> Path.basename()
    |> case do
      "." -> Path.basename(File.cwd!())
      "" -> "app"
      name -> name
    end
  end

  defp normalize_namespace(name) do
    normalized =
      name
      |> String.downcase()
      |> String.replace(~r/[^a-z0-9-]+/u, "-")
      |> String.replace(~r/-+/u, "-")
      |> String.trim("-")

    if normalized == "", do: "app", else: normalized
  end

  defp titleize(name) do
    name
    |> String.replace(~r/[-_]+/u, " ")
    |> String.split(~r/\s+/, trim: true)
    |> Enum.map_join(" ", &String.capitalize/1)
    |> case do
      "" -> "Arc App"
      value -> value
    end
  end

  defp arcfile_template do
    """
    version = 1

    [runtime]
    type = "exec"
    command = "./run.sh"
    cwd = "."

    [manifest]
    path = "./manifest.json"
    """
  end

  defp manifest_template(namespace, title) do
    """
    {
      "published_at": "2026-03-06T00:00:00Z",
      "release": {
        "version": "0.1.0",
        "channel": "stable"
      },
      "capability": {
        "id": "primary",
        "kind": "service",
        "scheme": "#{namespace}",
        "title": "#{title}",
        "summary": "Starter ARC app scaffold. Edit this manifest and runtime to expose your own capability.",
        "invocation": {
          "method": "RAW",
          "path": "/"
        },
        "examples": [
          "hello from arc"
        ]
      },
      "interfaces": {
        "cli": {
          "version": 1,
          "namespace": "#{namespace}",
          "summary": "Starter ARC app command surface",
          "commands": [
            {
              "path": [],
              "summary": "Send a message to the starter ARC app",
              "args": [
                {
                  "name": "message",
                  "kind": "positional",
                  "required": false,
                  "variadic": true,
                  "description": "Message text to send"
                }
              ],
              "input": {
                "source": "template",
                "template": "{{message}}"
              },
              "examples": [
                "hello from arc"
              ]
            }
          ]
        }
      }
    }
    """
  end

  defp runtime_template(namespace) do
    """
    #!/bin/sh
    set -eu

    namespace="#{namespace}"

    extract_field() {
      printf '%s\n' "$1" | awk -v key="$2" '
        {
          needle = "\\\"" key "\\\":\\\""
          start = index($0, needle)

          if (start > 0) {
            value = substr($0, start + length(needle))
            stop = index(value, "\\\"")

            if (stop > 0) {
              print substr(value, 1, stop - 1)
            }
          }
        }
      '
    }

    while IFS= read -r line; do
      request_id="$(extract_field "$line" request_id)"
      message="$(extract_field "$line" message)"

      # Simple shell starter. Good enough for hello-world traffic; replace this
      # with a real runtime for anything that needs robust JSON handling.
      if [ -n "$message" ]; then
        reply="hello from $namespace: $message"
      else
        reply="hello from $namespace"
      fi

      if [ -n "$request_id" ]; then
        printf '{"op":"reply","request_id":"%s","reply":"%s"}\n' "$request_id" "$reply"
      else
        printf '{"reply":"%s"}\n' "$reply"
      fi
    done
    """
  end

  defp present_string(value) when is_binary(value) and value != "", do: value
  defp present_string(_value), do: nil
end
