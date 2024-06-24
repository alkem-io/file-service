import { Controller, Get, HttpException, HttpStatus } from '@nestjs/common';
import { FileService } from '../file-reader';

@Controller('/health')
export class HealthController {
  constructor(private readonly fileService: FileService) {}
  @Get('/')
  public async getHello(): Promise<string> {
    if (!(await this.fileService.isConnected())) {
      throw new HttpException('unhealthy!', HttpStatus.INTERNAL_SERVER_ERROR);
    }

    return 'healthy!';
  }
}
