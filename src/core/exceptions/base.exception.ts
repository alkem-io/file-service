type StaticExceptionDetails = {
  originalException?: Error;
  // static fields to be added here, e.g. 'cause', 'userId'
};
export type ExceptionDetails = Record<string, unknown> & StaticExceptionDetails;

export class BaseException extends Error {
  public readonly name: string;
  constructor(
    public message: string,
    public details?: ExceptionDetails,
  ) {
    super(message);
    this.name = this.constructor.name;
  }
}
