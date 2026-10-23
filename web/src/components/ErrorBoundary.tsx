import { Component, type ErrorInfo, type ReactNode } from "react";

interface Props {
  children: ReactNode;
  inline?: boolean;
}

// Keeps one crashing section from blanking the whole app.
export class ErrorBoundary extends Component<Props, { error: Error | null }> {
  state = { error: null as Error | null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("UI crashed:", error, info.componentStack);
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className={this.props.inline ? "p-5" : "mx-auto max-w-lg p-8"}>
        <p className="text-sm font-medium">Something went wrong showing this section.</p>
        <p className="mt-1 break-all text-sm text-neutral-500">{this.state.error.message}</p>
        <button className="mt-3 text-sm underline" onClick={() => this.setState({ error: null })}>Try again</button>{" "}
        <button className="mt-3 text-sm underline" onClick={() => window.location.reload()}>Reload page</button>
      </div>
    );
  }
}
